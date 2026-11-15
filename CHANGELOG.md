# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The version is the same for the Go bridge, the Python MCP server and the Claude Code plugin
published in the [oleggohman/cc-things](https://github.com/oleggohman/cc-things) marketplace.

## [Unreleased]

### Fixed

- An FTS index with segments that have no `messages_fts_idx` entry is now detected. FTS5 finds a
  term through that table, so such a segment made search return no hits for its terms while both
  `integrity-check` forms and the row count passed. The start-up check now counts these segments
  first; on a hit the next start drops and rebuilds the index, and search uses `LIKE` meanwhile.
  The check cannot see a segment that keeps at least one idx row and lost others.
- `get_contact_chats` and `get_last_interaction` match a group JID (`...@g.us`) only to its own
  chat. Before, a group JID could also match chats through the contact and sender lookups.

### Changed

- When SQLite cannot drop a damaged `messages_fts` table, the bridge logs an error that names the
  new README Troubleshooting entry "Search index cannot be removed", with the manual steps. A
  second entry, "Search misses messages that exist", covers damage the start-up checks cannot see.
  Messages stay stored and search keeps working through `LIKE`.
- The bridge bundles go-sqlite3 1.14.53 (SQLite 3.54.1, was 1.14.52).
- The MCP server runs on mcp 2.x (`mcp[cli]>=2.3.0,<3`) and pulls in the new `mcp-types` and
  `opentelemetry-api` packages. The tools and their error texts are unchanged. The server now
  reports the release as its `serverInfo.version`.
- `list_messages` reads the hits and their context in one read transaction. This does not block
  bridge writes on a WAL database, which is what the bridge always creates. On a rollback-journal
  `messages.db` a bridge write waits until the call ends.

## [0.4.0] - 2026-11-15

### Changed

- Reads on a large `messages.db` are much faster. The bridge adds indexes on `messages` and an FTS5
  trigram index (`messages_fts`) kept up to date by triggers. The MCP server reuses one database
  connection per call instead of opening several per message.
- Every bridge build needs `-tags sqlite_fts5`; an untagged build does not compile. The MCP
  server's automatic build and the README commands already pass it.
- The first start after an upgrade builds the indexes and the FTS table after `/api/health` is
  listening and before WhatsApp connects. Search uses `LIKE` until that finishes. During that
  window `POST /api/reconnect`, `POST /api/start-pairing` and `POST /api/pair-phone` return 503
  `bridge is starting` and do not connect. `POST /api/send` keeps its "Not connected" answer.
  On later starts an FTS index that missed writes (a missing trigger, or fewer indexed rows than
  messages) is dropped before it is rebuilt, so search uses `LIKE` during the rebuild and after a
  failed one instead of silently missing messages. An index FTS5 can no longer open, or one that
  fails the FTS5 `integrity-check` (damaged segments, records or shadow tables), is treated the
  same way.
  Its triggers are dropped first, so writes to `messages` keep working even when the broken table
  itself cannot be dropped; the bridge then logs the error on each start, connects, and search
  stays on `LIKE`. Triggers left behind by a dropped `messages_fts` are removed before anything
  else, even when the start is cancelled.
- Every start runs the FTS5 `integrity-check` on an existing `messages_fts` before WhatsApp
  connects, in its content-checking form (`rank` 1): it re-reads every message and compares its
  tokens with the index, because the plain check misses damage that makes writes fail. It reads
  the whole index and `messages`, so it adds time to each start that grows with the archive
  (about 1.4 times the plain check; roughly 0.2 s per 100,000 short messages, half a full
  rebuild); `bridge.log` records how long it took.
- Text search: queries of 3 or more characters use FTS5 trigram and still match substrings
  (including Hebrew), with Unicode case folding. Shorter queries use `LIKE`. `%` and `_` in a
  query are now literal characters on every path; before they acted as `LIKE` wildcards. If
  `messages_fts` or any of its three triggers is missing, or `MATCH` fails, search falls back to
  `LIKE` and does not return an empty list.
- `list_messages` `after` / `before` are strict (`>` and `<`) and use the stored timestamp layout.
  The MCP server converts the bound to its own local time zone (a naive value is read as local
  time), so the MCP server and the bridge must run in the same time zone. Offset-aware values and
  fractional seconds now match. A naive
  bound equal to a stored second no longer includes that row. Ordering is by timestamp text: it
  matches chronology for rows with the same offset, and rows written across a DST fall-back hour
  can be out of order. A UTC migration is not part of this change.
- Sender display names no longer use `LIKE '%digits%'`: `15551212` does not take the name of
  `551212`. A `...@lid` sender still resolves when that exact chat row exists.
- The last message of a chat is the newest row by `timestamp DESC, rowid DESC`, not a join on
  `last_message_time = timestamp`. Chats with equal timestamps are no longer duplicated, and a chat
  whose `last_message_time` matches no message now shows its last text instead of nothing.
- `get_direct_chat_by_contact` matches `user@s.whatsapp.net` and `user@lid` exactly after removing
  `+`, spaces and dashes, instead of `LIKE '%phone%'`: `551212` no longer finds `15551212`. The
  `@s.whatsapp.net` chat wins when both exist. A phone number does not find an unmerged LID chat;
  the LID repair still merges those.
- `get_contact_chats` accepts `111` or `111@s.whatsapp.net` and returns one row per chat. It now
  includes direct chats that have no messages and no longer repeats a chat.
- `get_last_interaction` returns the newer of the sender lookup and the chat lookup, so a message
  the contact sent in a group can be returned over an older direct-chat message.
- History sync is stored in one transaction per conversation. One failed insert rolls the whole
  conversation back, and the warning is now `Failed to store history conversation` instead of the
  per-message and per-chat warnings. Per-message history lines moved from Info to Debug; one Info
  line per conversation remains.
- `chats.last_message_time` after a history conversation is `max(stored time, newest message in
  the batch)`, so an older chunk does not move a chat backward. Times are compared as text, so
  values written under different UTC offsets compare by text, not by instant. Across a DST change
  an older batch can compare as newer and move the time back by up to the offset difference.
- `messages.db` uses `synchronous=NORMAL`. A committed transaction survives a process crash; a
  power loss can drop the commits made since the last WAL checkpoint. The store is a local mirror.
- `messages` rows are updated in place (`ON CONFLICT DO UPDATE`) so the SQLite `rowid` stays
  stable for the FTS index.

### Downgrading

To run an older bridge on a store that was opened by this version, drop the FTS objects first. The
indexes can stay.

```sql
DROP TRIGGER IF EXISTS messages_fts_insert;
DROP TRIGGER IF EXISTS messages_fts_delete;
DROP TRIGGER IF EXISTS messages_fts_update;
DROP TABLE IF EXISTS messages_fts;
```

## [0.3.4] - 2026-11-13

### Changed

- Release archives are `.zip` files for every platform (macOS and Linux were `.tar.gz`), and the
  GitHub release notes are taken from this changelog.

## [0.3.3] - 2026-11-13

### Fixed

- On Windows, a document sent through the MCP got its whole file path as its title; it now gets
  the file name.
- On Windows, media download paths are also checked for backslash and drive-letter escapes from
  the store directory.
- The bridge closes its WhatsApp device database when it exits.
- The v0.3.2 release build failed because several bridge tests assumed Unix paths and file
  permissions. CI now runs the bridge tests on Windows too.

## [0.3.2] - 2026-11-13

### Fixed

- Messages sent through the MCP (`send_message`, `send_file`, `send_audio_message`) are now saved
  to the local message database, in the same chat and with the same sender as messages sent from
  the phone. Before, they were delivered but never stored, so `list_messages` and
  `get_last_interaction` did not show them.
- A bridge started by the previous plugin version is now replaced automatically after a plugin
  update. Before, the server only replaced bridges it had built in its own install directory,
  and every plugin version installs into a new one. A prebuilt bridge binary is never rebuilt
  over.
- The v0.3.1 release build failed on Windows because a test used a Unix-only system call.
- Lint findings reported by golangci-lint in CI. The bridge now logs a failure to store a chat
  during history sync instead of ignoring it.

### Security

- A new message store directory is created with mode `0700` instead of `0755`, because it holds
  the WhatsApp session, the API token and private messages. Existing directories keep their mode.

## [0.3.1] - 2026-11-13

### Security

- Updated `golang.org/x/net` to v0.62.0 and `golang.org/x/crypto` to v0.59.0, fixing
  CVE-2026-78661, CVE-2026-97045 and GO-2026-5932.

## [0.3.0] - 2026-11-13

### Added

- `POST /api/shutdown` on the bridge (token required) for a graceful, version-checked exit.
- `release` field in `/api/health`, so servers can tell an older bridge from a newer one.

### Changed

- After an update, the MCP server replaces a bridge built from older sources: it builds the new
  version while the old bridge keeps serving, asks the old bridge to shut down, and starts the
  new one. A bridge newer than the server is never replaced.

## [0.2.0] - 2026-11-12

### Added

- The bridge runs as a shared, headless daemon supervised by the MCP server, with a status and
  pairing API, a single-instance lock and an idle exit.
- Pairing tools in the MCP server (QR code and phone-number code).
- The bridge appears as **MCP-CC** (desktop) in WhatsApp's linked devices list.
- CI with 100% test coverage for the bridge and the server, Dependabot, and a tag-driven release
  workflow.

### Changed

- Upgraded Go and Python dependencies; the MCP server talks to the bridge through httpx.
- Search and chat names understand WhatsApp LID addressing.

### Security

- The REST server binds to loopback, requires a bearer token, guards requests and only allows
  safe download paths. Pairing and reconnect moved to guarded POST endpoints.
- Logs carry message metadata only, never message bodies or file names.

### Fixed

- Many reliability fixes in pairing, history sync, media handling and bridge supervision.

## [0.0.1] - 2025-04-06

- Initial version from the upstream project.

[Unreleased]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.4.0...HEAD
[0.4.0]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.4...v0.4.0
[0.3.4]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.3...v0.3.4
[0.3.3]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.2...v0.3.3
[0.3.2]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.0.1...v0.2.0
[0.0.1]: https://github.com/oleggohman/whatsapp-mcp/releases/tag/v0.0.1
