# WhatsApp MCP

[![CI](https://github.com/oleggohman/whatsapp-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/oleggohman/whatsapp-mcp/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/oleggohman/whatsapp-mcp)](https://github.com/oleggohman/whatsapp-mcp/releases)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

Let Claude read, search and send your personal WhatsApp messages.

WhatsApp MCP is a [Model Context Protocol](https://modelcontextprotocol.io) server for Claude Code, Claude Desktop and any other MCP client. It links to your own WhatsApp account as a device, the same way WhatsApp Web does, through the [whatsmeow](https://github.com/tulir/whatsmeow) library. Your message history stays on your machine in SQLite. A message reaches the model only when a tool call returns it.

```text
You:    What did Dana say about the flight on Sunday?
Claude: (calls search_contacts, then list_messages)
        Dana wrote at 18:42: "Landing 23:10, terminal 3. Can you pick me up?"
You:    Tell her yes, I'll be there at 23:00.
Claude: (calls send_message after you approve it)
```

Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) (MIT).

> [!CAUTION]
> Like most MCP servers that read private data and can send messages, this one is exposed to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). A received message can carry instructions aimed at the model. Keep per-tool approval on and review every `send_message` and `send_file` call. See [Security and privacy](#security-and-privacy).

## Contents

- [Features](#features)
- [Install](#install)
- [Link your WhatsApp account](#link-your-whatsapp-account)
- [Tools](#tools)
- [Configuration](#configuration)
- [Security and privacy](#security-and-privacy)
- [How it works](#how-it-works)
- [Troubleshooting](#troubleshooting)
- [Upgrading from lharries/whatsapp-mcp](#upgrading-from-lharrieswhatsapp-mcp)
- [Development](#development)
- [License](#license)

## Features

| | |
| --- | --- |
| **Search and read** | Find contacts, chats and messages by name, phone number, text, date range or sender. Fetch the messages around any message. Chats that WhatsApp keeps under hidden LID identifiers are found by phone number too. |
| **Send** | Text to people and groups, files (images, video, documents) and voice notes. With `ffmpeg` installed, audio is converted to Opus for you. |
| **Media** | Download the media of any received message and get its local path. |
| **One server to register** | The MCP server builds, starts and reuses the Go bridge itself. Claude Code and Claude Desktop share one bridge. |
| **Pair from the chat** | Link your account with a QR code image or a phone-number code. No terminal needed. |
| **Names from macOS Contacts** | When WhatsApp has no name for a number, the server looks it up in the Contacts app (read-only, local, in memory). WhatsApp names always win. |
| **Fast search on large archives** | An FTS5 trigram index makes text search fast and still matches substrings. |
| **Local and locked down** | The bridge listens on `127.0.0.1` only, checks a token on every call and rejects browser requests. |

## Install

Pick one of the three options. All of them need:

- Python 3.11 or newer and [uv](https://docs.astral.sh/uv/) (`curl -LsSf https://astral.sh/uv/install.sh | sh`).
- To build the bridge from source (options 1 and 3): Go 1.27 and a C compiler, because the SQLite driver uses cgo. macOS: Xcode command line tools. Linux: `gcc`. Windows: [MSYS2](https://www.msys2.org/) with `ucrt64\bin` on your `PATH`.
- Optional: `ffmpeg`, only to send audio in other formats as voice notes.

### Option 1: Claude Code plugin (recommended)

The plugin is published in the [oleggohman/cc-things](https://github.com/oleggohman/cc-things) marketplace. In Claude Code run:

```text
/plugin marketplace add oleggohman/cc-things
/plugin install whatsapp@oleggohman-cc-things
```

Start a new Claude Code session. The plugin keeps its store in the plugin data directory (`~/.claude/plugins/data/whatsapp-*/store`). To update later, run `/plugin marketplace update oleggohman-cc-things`, then update the plugin and start a new session.

### Option 2: release archive (no Go needed)

1. From the [Releases page](https://github.com/oleggohman/whatsapp-mcp/releases), download the `.zip` for your platform (`darwin-arm64`, `darwin-amd64`, `linux-amd64`, `windows-amd64`) and `SHA256SUMS`.
2. Verify it: `sha256sum -c SHA256SUMS --ignore-missing` (macOS: `shasum -a 256 -c SHA256SUMS --ignore-missing`).
3. Unzip it. The prebuilt bridge is `whatsapp-bridge/whatsapp-bridge`.
4. macOS only: the binary is unsigned, so Gatekeeper blocks it. Clear the quarantine flag once:

   ```bash
   xattr -dr com.apple.quarantine /path/to/whatsapp-mcp
   ```

5. [Register the server](#register-the-server-options-2-and-3).

### Option 3: from source

```bash
git clone https://github.com/oleggohman/whatsapp-mcp.git
cd whatsapp-mcp
```

Then [register the server](#register-the-server-options-2-and-3). The first start builds the bridge, which takes 1-2 minutes. The bridge is rebuilt whenever its sources are newer than the binary.

### Register the server (options 2 and 3)

Replace `/path/to/whatsapp-mcp` with your directory. Use the absolute path from `which uv` when your client cannot find `uv`.

**Claude Code**

```bash
claude mcp add whatsapp -- uv --directory /path/to/whatsapp-mcp/whatsapp-mcp-server run main.py
```

Add environment variables with `-e`, for example `claude mcp add whatsapp -e WHATSAPP_GO_BIN=/usr/local/go/bin/go -- uv ...`.

**Claude Desktop** (macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`), Cursor and other clients:

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/absolute/path/to/uv",
      "args": ["--directory", "/path/to/whatsapp-mcp/whatsapp-mcp-server", "run", "main.py"],
      "env": {
        "WHATSAPP_GO_BIN": "/opt/homebrew/bin/go"
      }
    }
  }
}
```

The `env` block is optional. Desktop apps often start with a short `PATH` that has no Go. The server also probes `/opt/homebrew/bin`, `/usr/local/go/bin`, `/usr/local/bin`, `~/go/bin` and `~/sdk/*/bin`. Set `WHATSAPP_FFMPEG_BIN` the same way if `ffmpeg` is not found.

Restart the client, then link your account.

## Link your WhatsApp account

The bridge becomes a **linked device** on your account, like WhatsApp Web. There is no password or API key: you approve the link on your phone once. The session is kept in `whatsapp.db` in the store directory. On your phone the bridge shows up under **Linked devices** as **MCP-CC**.

1. **Check the connection.** Ask Claude: *"Check my WhatsApp connection."* Claude calls `get_connection_status`. Wait for `state: running`. If `logged_in` is already `true`, you are done.
2. **Pair**, using one of:
   - **QR code.** Ask: *"Show me the WhatsApp pairing QR code."* On your phone open WhatsApp, then **Settings** (iOS) or the **⋮** menu (Android), then **Linked devices**, then **Link a device**, and scan the code. Codes rotate about every 20 seconds; ask for a fresh one if the scan fails. The Claude Code terminal may not show images, so use the phone-number code there.
   - **Phone-number code.** Ask: *"Pair WhatsApp with +1 555 0100."* Claude calls `pair_with_phone_number` with your number in international format, digits only (`15550100`). On your phone open **Linked devices**, then **Link a device**, then **Link with phone number instead**, and type the 8-character code.
3. **Confirm.** Ask Claude to check the connection again. `logged_in` and `connected` should both be `true`. The first history sync takes several minutes. Messages and chat names fill in as it runs.

`get_connection_status` returns:

| Field | Meaning |
| --- | --- |
| `state` | Bridge process: `building`, `starting`, `running` or `failed`. On `failed`, `log_tail` holds the end of the bridge log. |
| `connected` | The bridge has a live connection to WhatsApp. |
| `logged_in` | The bridge is linked to an account. |
| `pairing` | `idle`, `qr` (QR code active), `code` (phone-number code active) or `replaced` (another process took over the login). |
| `restart_needed` | The running bridge was built from other sources and was not replaced automatically. `hint` says what to do. |

**You need to link again when** you log the device out on your phone, WhatsApp expires the session (for example after the phone was offline for a long time), or `pairing` shows `replaced`. In the last case a second client connected with the same login. The bridge does not reconnect by itself, because two bridges would keep kicking each other off. Stop the other process, then call `get_connection_status` with `reconnect: true`.

## Tools

| Tool | Parameters (defaults) | Returns |
| --- | --- | --- |
| `search_contacts` | `query` | Contacts whose WhatsApp name, number or (macOS) Contacts name matches. |
| `list_messages` | `after`, `before` (ISO-8601, exclusive, local time without an offset), `sender_phone_number`, `chat_jid`, `query`, `limit` (20), `page` (0), `include_context` (true), `context_before` (1), `context_after` (1) | Matching messages as text, with surrounding context. |
| `list_chats` | `query`, `limit` (20), `page` (0), `include_last_message` (true), `sort_by` (`last_active` or `name`) | Chats with metadata. `query` and `sort_by` use the stored WhatsApp names. |
| `get_chat` | `chat_jid`, `include_last_message` (true) | One chat's metadata, or nothing. |
| `get_direct_chat_by_contact` | `sender_phone_number` | The direct chat with that number, or nothing. |
| `get_contact_chats` | `jid` (JID or bare number), `limit` (20), `page` (0) | All chats involving the contact. |
| `get_last_interaction` | `jid` (JID or bare number) | The latest message with the contact. |
| `get_message_context` | `message_id`, `before` (5), `after` (5) | A message and its neighbors. |
| `send_message` | `recipient`, `message` | `success`, `message`. |
| `send_file` | `recipient`, `media_path` (absolute) | `success`, `message`. |
| `send_audio_message` | `recipient`, `media_path` (absolute) | `success`, `message`. Sends a voice note; non-`.ogg` input is converted with `ffmpeg`. |
| `download_media` | `message_id`, `chat_jid` | `success`, `message`, `file_path`. |
| `get_connection_status` | `reconnect` (false) | `state`, `connected`, `logged_in`, `pairing`, `restart_needed`, `log_tail`. |
| `get_pairing_qr` | none | A QR code image. Starts a pairing session if none is active. |
| `pair_with_phone_number` | `phone` (international, digits only) | An 8-character pairing code. |

- `recipient` is a phone number with country code and no `+` or symbols, or a JID such as `15550100@s.whatsapp.net`, or a group JID ending in `@g.us`.
- The read tools query `messages.db` directly and work without a running bridge. Send, download and pairing need the bridge; the send tools answer "not paired" when no account is linked.
- Media is stored as metadata only. Call `download_media` with the `message_id` and `chat_jid` from the listing to fetch the file.

## Configuration

No variable is required. The bridge and the server read the same names. When the server starts the bridge it passes `WHATSAPP_STORE_DIR` on, and the bridge inherits the rest of the environment.

| Variable | Default | Used by | Meaning |
| --- | --- | --- | --- |
| `WHATSAPP_STORE_DIR` | `whatsapp-bridge/store` (bridge alone: `store` in its working directory) | both | Databases, token, lock, log and downloaded media. |
| `WHATSAPP_API_BASE_URL` | `http://localhost:8080/api` | server | Where the server reaches the bridge. |
| `WHATSAPP_BRIDGE_ADDR` | from the API URL (`127.0.0.1:8080`) | both | Listen address of the bridge API. |
| `WHATSAPP_BRIDGE_TOKEN` | token file | both | API token. Overrides `<store>/bridge.token`. |
| `WHATSAPP_BRIDGE_IDLE_EXIT` | unset (never exits) | bridge | Go duration such as `30m`. The bridge exits after this long without API calls. |
| `WHATSAPP_BRIDGE_AUTOSTART` | `1` | server | `0`, `false`, `no` or `off` turns off build and start ([manual mode](#manual-two-process-mode)). |
| `WHATSAPP_MACOS_CONTACTS` | `1` | server | `0`, `false`, `no` or `off` turns off the Contacts name fallback. |
| `WHATSAPP_BRIDGE_BIN` | `whatsapp-bridge/whatsapp-bridge` | server | Bridge binary to run, and where builds go. |
| `WHATSAPP_BRIDGE_SRC` | `whatsapp-bridge` | server | Bridge sources used for builds. |
| `WHATSAPP_BRIDGE_START_TIMEOUT` | `60` | server | Seconds to wait for a new bridge to answer. |
| `WHATSAPP_GO_BIN` | `go` on `PATH`, then common install paths | server | Go toolchain for builds. |
| `WHATSAPP_FFMPEG_BIN` | `ffmpeg` on `PATH`, then `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin` | server | `ffmpeg` for audio conversion. |
| `WHATSAPP_DB_PATH` | `<store>/messages.db` | server | `messages.db` used for reads. |
| `WHATSAPP_LOG_LEVEL` | `INFO` | server | Python log level. Logs go to stderr. |

## Security and privacy

- **Loopback only.** The bridge API binds to `127.0.0.1` and accepts only a `Host` of `localhost`, a loopback IP or the bind host, which blocks DNS rebinding.
- **No browser access.** Requests with an `Origin` header are rejected. `POST` bodies must be `application/json`, and `GET` never changes state.
- **Token on every call.** All endpoints except `/api/health` need the bearer token. The server creates it on first start as `<store>/bridge.token` (mode `0600`); `WHATSAPP_BRIDGE_TOKEN` overrides it. An empty token file stops the bridge from starting: delete it and the server makes a new one. A bridge you start by hand without a token file runs without authentication. This token has nothing to do with your WhatsApp login.
- **Downloads stay in the store.** Media is written below `<store>/<chat>/` only. Chat and file names from senders are reduced to safe bare names.
- **No message bodies in logs.**
- **Contacts stay local.** macOS Contacts are read from `~/Library/Application Support/AddressBook` read-only, kept in memory and never stored or sent. macOS asks once for Contacts access for the app that runs the server; refusing only disables the fallback.
- **Nothing to commit by accident.** The store, databases, token and logs are in `.gitignore`.

**Prompt injection is not blocked.** A received message can tell the model to call `send_file` on a sensitive file (an SSH key, a `.env`) or forward your chats. Nothing in this server stops that. Keep per-tool approval on in your client and read every send call before you approve it.

## How it works

```text
MCP client (Claude) <--stdio--> Python MCP server <--REST, 127.0.0.1--> Go bridge (whatsmeow) <--> WhatsApp
                                       |                                       |
                                       +--- reads (read-only) --- SQLite store: messages.db, whatsapp.db
```

- **Go bridge** (`whatsapp-bridge/`) connects to WhatsApp, handles pairing and writes history to `messages.db`. Its REST API: `/api/health`, `/api/status`, `/api/start-pairing`, `/api/pair-phone`, `/api/reconnect`, `/api/send`, `/api/download`, `/api/shutdown`. `GET /api/status` is read-only; every change is a `POST`.
- **Python MCP server** (`whatsapp-mcp-server/`) implements the tools. It reads `messages.db` read-only and calls the bridge to send, download and pair.
- **Store directory** (default `whatsapp-bridge/store`) holds `messages.db`, `whatsapp.db` (the whatsmeow session), `bridge.token`, `bridge.lock`, `bridge.log` and downloaded media.

### Bridge lifecycle

The bridge is a shared daemon, one per store directory. Any MCP client starts it when none is running, and no client stops it, so it outlives the client and serves Claude Code and Claude Desktop at once. Its log is `<store>/bridge.log`.

- **Stop it** with `WHATSAPP_BRIDGE_IDLE_EXIT` (for example `30m`). Only bridge calls count as activity: send, download, pairing and `get_connection_status`. Or kill it: `curl -s http://127.0.0.1:8080/api/health` shows its `pid`.
- **Restart** happens on the next tool call that needs the bridge. A failed start is retried after 30 seconds.
- **Reads after an idle exit** show history only up to the exit, because the read tools never start the bridge. Call `get_connection_status` first when you need fresh messages.
- **After an update**, restart the MCP server (in Claude Code, reconnect it under `/mcp`). The server sees that the running bridge is older, builds the new one while the old one keeps serving, asks the old one to stop (`POST /api/shutdown`, or `SIGTERM` for bridges older than 0.3.0 after checking the token) and starts the new one. The WhatsApp login is kept. Another open session loses at most one call. A bridge that reports a newer `release` than the server is never replaced; the hint asks you to update the older session.
- **No automatic replacement** while a pairing runs, when Go is missing, on Windows, or when the bridge stops mid-session. `get_connection_status` then shows `restart_needed: true` and a hint. Kill the daemon and the next bridge call rebuilds it. A prebuilt release bridge reports its release tag instead of a source hash and is never replaced.
- **One bridge per login.** The bridge locks `<store>/bridge.lock` and exits with code 75 when another bridge holds it. The lock does not cover copies of a store: a second process with the same login makes WhatsApp drop the first, which then reports `pairing: "replaced"`.

### Storage and search

- The bridge indexes `messages` by chat, timestamp and sender, and keeps an FTS5 trigram index (`messages_fts`) in step through triggers. Searches of 3 or more characters use it and still match substrings. Shorter queries, or a missing or broken index, fall back to `LIKE`.
- The first start after an upgrade builds the indexes after `/api/health` is up and before connecting to WhatsApp. Until it finishes, search uses `LIKE` and `POST /api/reconnect`, `/api/start-pairing` and `/api/pair-phone` return 503 `bridge is starting`.
- Every later start runs the FTS5 `integrity-check` (with `rank` 1, so it also compares the index with `messages`) and checks for orphaned index segments, which would make search silently miss terms. This reads the whole archive, about 0.2 s per 100,000 short messages (`bridge.log` records the time). A damaged index is dropped and rebuilt.
- `messages.db` runs in WAL mode with `synchronous=NORMAL`: a committed write survives a process crash, while a power loss can drop commits since the last checkpoint.
- `list_messages` reads hits and context in one read transaction, so a LID-chat merge cannot split them.
- The bridge must be built with `-tags sqlite_fts5`, as the automatic build does. An untagged build does not compile.

### LID chats

WhatsApp sometimes stores a chat under a hidden LID instead of a phone number. After connecting and after each history sync, the bridge merges chats named `LID <digits>` into the phone-number chat once WhatsApp provides the mapping. A chat for which WhatsApp gives neither a name nor a number keeps the name `LID <digits>`.

### Manual two-process mode

Set `WHATSAPP_BRIDGE_AUTOSTART=0` for the server, then run the bridge yourself:

```bash
cd whatsapp-bridge
CGO_ENABLED=1 go run -tags sqlite_fts5 .      # or build it and run the binary
```

When stderr is a terminal the bridge prints a pairing QR code right away; otherwise pair through the tools. Use the same `WHATSAPP_STORE_DIR`, `WHATSAPP_BRIDGE_ADDR` and token on both sides. In this mode the server never builds or restarts the bridge and never reports `restart_needed`. On Windows enable cgo once with `go env -w CGO_ENABLED=1`.

## Troubleshooting

- **The client cannot find `uv`.** Use the absolute path from `which uv` as `command`.
- **`state: failed` or build errors.** Read `log_tail` in `get_connection_status`, or the full `<store>/bridge.log`. Check that Go 1.27 and a C compiler are installed, and set `WHATSAPP_GO_BIN` if the client cannot find Go. On Windows a missing C compiler shows as `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`
- **Stuck on `building`.** The first build takes 1-2 minutes. Later starts reuse the binary.
- **"port in use by another bridge/store".** A bridge for another store uses the same port. Set `WHATSAPP_STORE_DIR` and `WHATSAPP_BRIDGE_ADDR` (with a matching `WHATSAPP_API_BASE_URL`) the same way in every client.
- **Cowork / Claude Desktop passes `${CLAUDE_PLUGIN_DATA}` unexpanded.** Since 0.4.5 a `WHATSAPP_STORE_DIR` that still contains `${...}` counts as unset. The server then uses, in order: the store of the bridge already running on the loopback address (only if you own the dir, it is not group- or other-writable, and its `bridge.token` is mode 0600); the single Claude Code store `~/.claude/plugins/data/whatsapp-*/store` that holds `whatsapp.db`; else `~/.whatsapp-mcp/store`. The choice is made once per process, so restart Cowork if it fell back to `~/.whatsapp-mcp/store` before another bridge started. Set an absolute `WHATSAPP_STORE_DIR` to skip the fallback.
- **"bridge running with unknown token".** The running bridge has another token. Set `WHATSAPP_BRIDGE_TOKEN` to it, or kill the bridge (pid from `/api/health`) and let the server start a new one.
- **`pairing: "replaced"`.** Another process uses the same login. Stop it, then call `get_connection_status` with `reconnect: true`. Without MCP: `curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $(cat <store>/bridge.token)" -d "{}" http://127.0.0.1:8080/api/reconnect`.
- **QR code expired.** Codes rotate about every 20 seconds. Call `get_pairing_qr` again or use `pair_with_phone_number`.
- **Device limit reached.** Remove a device under **Linked devices** in WhatsApp.
- **No messages after pairing.** The history sync takes several minutes.
- **New messages missing.** The bridge may have exited after `WHATSAPP_BRIDGE_IDLE_EXIT`. Call `get_connection_status` to start it.
- **Contacts names missing on macOS.** Allow Contacts access for the app that runs the server under System Settings > Privacy & Security > Contacts, then call any read tool again (the server retries after 60 s). With `WHATSAPP_LOG_LEVEL=INFO` the server logs one `macOS Contacts not readable` line when access is denied.
- **Search index cannot be removed.** `bridge.log` says the `messages_fts` table is damaged and SQLite cannot drop it. Messages are still stored and search falls back to `LIKE`, only slower. To clear it by hand:
  1. Stop the bridge.
  2. Copy `messages.db`, `messages.db-wal` and `messages.db-shm` somewhere safe.
  3. Run in `sqlite3 <store>/messages.db`:

     ```sql
     DROP TRIGGER IF EXISTS messages_fts_insert;
     DROP TRIGGER IF EXISTS messages_fts_delete;
     DROP TRIGGER IF EXISTS messages_fts_update;
     PRAGMA writable_schema=ON;
     DELETE FROM sqlite_master WHERE type = 'table' AND name = 'messages_fts' AND rootpage = 0;
     PRAGMA writable_schema=RESET;
     DROP TABLE IF EXISTS messages_fts_data;
     DROP TABLE IF EXISTS messages_fts_idx;
     DROP TABLE IF EXISTS messages_fts_docsize;
     DROP TABLE IF EXISTS messages_fts_config;
     PRAGMA integrity_check;
     ```

  4. `integrity_check` must print `ok`. Start the bridge and it rebuilds the index. If it does not print `ok`, restore the copy and follow "Out of sync".
- **Search misses messages that exist.** Stop the bridge and run `DROP TRIGGER IF EXISTS messages_fts_insert;`, `DROP TRIGGER IF EXISTS messages_fts_delete;`, `DROP TRIGGER IF EXISTS messages_fts_update;` and `DROP TABLE IF EXISTS messages_fts;` in `sqlite3 <store>/messages.db`. Start the bridge. This forces a rebuild and covers damage the start-up checks cannot see.
- **Out of sync.** Stop the bridge. Delete `messages.db*` and `whatsapp.db*` in the store directory, including the `-wal` and `-shm` files. This unlinks the device. Restart and pair again.

For Claude Desktop issues see also the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).

## Upgrading from lharries/whatsapp-mcp

1. **Back up `whatsapp-bridge/store/`** (or your `WHATSAPP_STORE_DIR`). The new bridge migrates `whatsapp.db` on first run, and the migration is one-way: older versions cannot read the result.
2. **Stop the old bridge.** Only one bridge may run per WhatsApp login.
3. [Register the server](#register-the-server-options-2-and-3). It replaces the old `go run main.go` workflow and starts the bridge for you.

## Development

```bash
cd whatsapp-bridge && go test -tags sqlite_fts5 ./... && golangci-lint run
cd whatsapp-mcp-server && uv run pytest && uv run ruff check .
```

The bridge, the MCP server and the Claude Code plugin share one version. Changes are listed in [CHANGELOG.md](CHANGELOG.md).

### Releasing (maintainers)

1. Set the version in `whatsapp-mcp-server/pyproject.toml` (and `uv.lock`), `RELEASE` in `whatsapp-mcp-server/bridge_manager.py`, and `bridgeVersion` and `bridgeRelease` in `whatsapp-bridge/pairing.go`. A test fails if they differ.
2. Move the `Unreleased` entries in [CHANGELOG.md](CHANGELOG.md) under the new version and date.
3. Push a tag. The Release workflow runs the tests, builds the bridge for macOS (arm64, amd64), Linux and Windows, and publishes a GitHub release with one `.zip` per platform, `SHA256SUMS`, and the version's CHANGELOG section as notes. It fails if that section is missing.

   ```bash
   git tag vX.Y.Z && git push origin vX.Y.Z
   ```

4. Bump the `whatsapp` entry in the [cc-things](https://github.com/oleggohman/cc-things) marketplace, update the plugin and start a new Claude Code session.
5. Validate against a live account with the project skill [`validate-whatsapp-mcp`](.claude/skills/validate-whatsapp-mcp/SKILL.md) (`/validate-whatsapp-mcp` in Claude Code). It calls every tool the server lists and reports one row per tool. Send tools run only with your OK and only to your own "Message yourself" chat. A release is done when no tool is ❌; fix any ❌ in a follow-up release.

## License

MIT. See [LICENSE](LICENSE). Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).
