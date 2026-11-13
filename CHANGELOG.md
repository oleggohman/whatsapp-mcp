# Changelog

All notable changes to this project are documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project
uses [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The version is the same for the Go bridge, the Python MCP server and the Claude Code plugin
published in the [oleggohman/cc-things](https://github.com/oleggohman/cc-things) marketplace.

## [Unreleased]

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

[Unreleased]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.2...HEAD
[0.3.2]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.1...v0.3.2
[0.3.1]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/oleggohman/whatsapp-mcp/compare/v0.0.1...v0.2.0
[0.0.1]: https://github.com/oleggohman/whatsapp-mcp/releases/tag/v0.0.1
