# WhatsApp MCP Server

[![CI](https://github.com/oleggohman/whatsapp-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/oleggohman/whatsapp-mcp/actions/workflows/ci.yml)

A Model Context Protocol (MCP) server that lets Claude (Claude Desktop, Claude Code) or any other MCP client search and read your personal WhatsApp messages and send messages and media. It connects to your own WhatsApp account as a linked device through WhatsApp's multi-device API, using the [whatsmeow](https://github.com/tulir/whatsmeow) library. Your messages are stored locally in SQLite. They reach the language model only when a tool returns them.

This project is a fork of [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp) (MIT license).

> **Caution:** like most MCP servers, this one is exposed to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/): a prompt-injected model could leak private data. See [Security and privacy](#security-and-privacy).

## Contents

- [Main features](#main-features)
- [Quick start](#quick-start)
- [Authorization (linking your WhatsApp account)](#authorization-linking-your-whatsapp-account)
- [Tools reference](#tools-reference)
- [How it works](#how-it-works)
- [Configuration](#configuration)
- [Security and privacy](#security-and-privacy)
- [Upgrading from the original version](#upgrading-from-the-original-lharries-version)
- [Troubleshooting](#troubleshooting)
- [Development](#development)

## Main features

- **Search and read.** Find contacts, chats and messages by name, phone number, text, date range or sender. Chats that WhatsApp stores under hidden LID identifiers are found by phone number too.
- **Message context.** Fetch the messages before and after any message.
- **Send messages.** Send text to people and groups, and send files (images, video, documents).
- **Voice notes.** Send audio as a playable voice message. With `ffmpeg` installed, other formats are converted to Opus automatically.
- **Download media.** Download the media of a received message and get the local file path.
- **Single-MCP setup.** You register one server. It builds, starts and reuses the Go bridge for you. Claude Desktop and Claude Code share the same bridge.
- **Pairing from inside Claude.** Link your account with a QR code image or a phone-number code. No terminal needed.
- **Local, token-protected bridge API.** The bridge listens on `127.0.0.1` only and requires a token.
- **Automatic repair of LID-named chats.** The bridge merges chats stored as `LID <digits>` into the phone-number chat once WhatsApp provides the mapping.

## Quick start

### Prerequisites

- Go 1.27 and a C compiler (the SQLite driver needs cgo). Not needed with a release archive. On macOS install the Xcode command line tools, on Linux `gcc`, on Windows [MSYS2](https://www.msys2.org/) with `ucrt64\bin` on your `PATH`.
- Python 3.11 or newer.
- [uv](https://docs.astral.sh/uv/) (`curl -LsSf https://astral.sh/uv/install.sh | sh`).
- `ffmpeg` (optional). Only needed to convert audio to voice notes.

### Option A: release archive (no Go needed)

1. Download the archive for your OS from the [Releases page](https://github.com/oleggohman/whatsapp-mcp/releases) (`darwin-arm64`, `darwin-amd64`, `linux-amd64` or `windows-amd64`), together with `SHA256SUMS`.
2. Verify it: `sha256sum -c SHA256SUMS --ignore-missing` (macOS: `shasum -a 256 -c SHA256SUMS --ignore-missing`).
3. Extract it. The prebuilt bridge is at `whatsapp-bridge/whatsapp-bridge`.
4. On macOS the binary is unsigned and Gatekeeper blocks it. Remove the quarantine flag once:

   ```bash
   xattr -dr com.apple.quarantine /path/to/whatsapp-mcp
   ```

### Option B: from source

```bash
git clone https://github.com/oleggohman/whatsapp-mcp.git
cd whatsapp-mcp
```

### Connect to Claude

Run `which uv` to get the absolute path of `uv`. Replace `/path/to/whatsapp-mcp` with the directory from option A or B.

**Claude Desktop.** Edit `claude_desktop_config.json` (macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`):

```json
{
  "mcpServers": {
    "whatsapp": {
      "command": "/absolute/path/to/uv",
      "args": [
        "--directory",
        "/path/to/whatsapp-mcp/whatsapp-mcp-server",
        "run",
        "main.py"
      ],
      "env": {
        "WHATSAPP_GO_BIN": "/opt/homebrew/bin/go"
      }
    }
  }
}
```

The `env` block is optional. Add `WHATSAPP_GO_BIN` only when the desktop app starts with a minimal `PATH` that does not contain Go. The server also probes `/opt/homebrew/bin`, `/usr/local/go/bin`, `/usr/local/bin`, `~/go/bin` and `~/sdk/*/bin`. Set `WHATSAPP_FFMPEG_BIN` the same way when `ffmpeg` is not found.

**Claude Code.**

```bash
claude mcp add whatsapp -- uv --directory /path/to/whatsapp-mcp/whatsapp-mcp-server run main.py
# with environment variables:
claude mcp add whatsapp -e WHATSAPP_GO_BIN=/usr/local/go/bin/go -- uv --directory /path/to/whatsapp-mcp/whatsapp-mcp-server run main.py
```

Other clients (for example Cursor) take the same `mcpServers` block.

Restart the client. The first start builds the bridge from source, which takes about 1-2 minutes. `get_connection_status` reports `building` meanwhile. The bridge is rebuilt when its sources are newer than the binary. Then continue with [Authorization](#authorization-linking-your-whatsapp-account).

## Authorization (linking your WhatsApp account)

### What it is

The bridge becomes a **linked device** on your WhatsApp account, the same mechanism as WhatsApp Web. There is no password or API key. You approve the link on your phone, which needs WhatsApp and an internet connection during pairing. You do this once. The session is stored in `whatsapp.db` in the store directory. In the phone's **Linked devices** list the bridge appears as **MCP-CC** with a desktop icon. A bridge linked before this name was introduced keeps its old name until you log it out and pair again.

### Step 1: check the connection

Ask Claude: "Check my WhatsApp connection." Claude calls `get_connection_status`. The result has these fields:

| Field | Meaning |
| --- | --- |
| `state` | Bridge process state: `building`, `starting`, `running` or `failed`. On `failed`, `log_tail` holds the bridge log. |
| `connected` | The bridge has a live connection to WhatsApp. |
| `logged_in` | The bridge is linked to a WhatsApp account. |
| `pairing` | `idle` (nothing in progress), `qr` (a QR code is active), `code` (a phone-number code is active) or `replaced` (another process took over the login). |
| `restart_needed` | The running bridge is older than the sources. See [Bridge lifecycle](#bridge-lifecycle). |

Wait for `state: running`. If `logged_in` is already `true`, you are done.

### Step 2a: pair with a QR code

1. Ask Claude: "Show me the WhatsApp pairing QR code." Claude calls `get_pairing_qr`, which returns an image.
2. On your phone open WhatsApp: **Settings** on iOS, the **⋮ menu** on Android. Then choose **Linked devices**, then **Link a device**.
3. Scan the QR code.

The code rotates about every 20 seconds. If the scan fails, ask Claude for a fresh one. The Claude Code terminal may not render images. Use step 2b there.

### Step 2b: pair with a phone-number code

1. Ask Claude to pair with your number, for example: "Pair WhatsApp with +1 555 0100." Claude calls `pair_with_phone_number` with the number in international format, digits only, without `+` and without a leading `0` (for example `15550100`).
2. The tool returns an 8-character code.
3. On your phone open **Linked devices**, then **Link a device**, then **Link with phone number instead**, and enter the code.

### Step 3: confirm

Ask Claude to check the connection again. `logged_in` and `connected` should both be `true`. The first history sync takes several minutes. Messages arrive over that time and chat names fill in as the sync progresses.

### When you need to link again

- You logged the device out from your phone (**Linked devices**, then the device, then **Log out**). The bridge then reports `logged_in: false`.
- WhatsApp expired the session, for example after the phone stayed offline for a long time.
- `pairing` shows `replaced`. A second bridge or another client connected with the same login, and WhatsApp disconnected this one. The bridge does not reconnect on its own, because two bridges would kick each other off forever. Stop the other process, then call `get_connection_status` with `reconnect: true`.

### Manual alternative

Run the bridge in a terminal. It prints the pairing QR code there. See [Manual two-process mode](#manual-two-process-mode).

### The local bridge token

The bridge API has its own access token. It is unrelated to your WhatsApp login. The MCP server generates it on first start as `<store>/bridge.token` (mode `0600`). Every bridge API call except `/api/health` sends it. You normally never touch it. `WHATSAPP_BRIDGE_TOKEN` overrides the file. If `bridge.token` exists but is empty, the bridge refuses to start. Delete the file and the server creates a new one.

## Tools reference

| Tool | Key parameters | Returns |
| --- | --- | --- |
| `search_contacts` | `query` | Contacts whose name or phone number matches. |
| `list_messages` | `after`, `before` (ISO-8601), `sender_phone_number`, `chat_jid`, `query`, `limit` (20), `page` (0), `include_context` (true), `context_before` (1), `context_after` (1) | Matching messages as formatted text, with surrounding context. |
| `list_chats` | `query`, `limit` (20), `page` (0), `include_last_message` (true), `sort_by` (`last_active` or `name`) | Chats with metadata. |
| `get_chat` | `chat_jid`, `include_last_message` (true) | Metadata of one chat, or nothing. |
| `get_direct_chat_by_contact` | `sender_phone_number` | The direct chat with that number, or nothing. |
| `get_contact_chats` | `jid`, `limit` (20), `page` (0) | All chats that involve the contact. |
| `get_last_interaction` | `jid` | The most recent message with the contact. |
| `get_message_context` | `message_id`, `before` (5), `after` (5) | The message and its neighbors. |
| `send_message` | `recipient`, `message` | `success` and `message`. |
| `send_file` | `recipient`, `media_path` (absolute path) | `success` and `message`. |
| `send_audio_message` | `recipient`, `media_path` (absolute path) | `success` and `message`. Sends a voice note and converts non-`.ogg` input with `ffmpeg`. |
| `download_media` | `message_id`, `chat_jid` | `success`, `message` and `file_path`. |
| `get_connection_status` | `reconnect` (false) | Bridge `state`, `connected`, `logged_in`, `pairing`, `restart_needed`, `log_tail`. |
| `get_pairing_qr` | none | A QR code image to scan. Starts a pairing session if none is active. |
| `pair_with_phone_number` | `phone` (international format, digits only) | An 8-character pairing code. |

`recipient` is a phone number with country code and no `+` or other symbols, or a JID such as `15550100@s.whatsapp.net` or a group JID ending in `@g.us`. The read tools query `messages.db` directly and do not need the bridge to run. The send, download and pairing tools need a running bridge, and the send tools return a "not paired" message when the account is not linked.

Media is stored as metadata only. To get the file, call `download_media` with the `message_id` and `chat_jid` shown in the message listing.

## How it works

```text
MCP client (Claude)  <-- stdio -->  Python MCP server  <-- REST, localhost -->  Go bridge (whatsmeow)  <-->  WhatsApp
                                          |                                           |
                                          +------ reads (read-only) ------ SQLite store: messages.db, whatsapp.db
```

1. **Go bridge** (`whatsapp-bridge/`) connects to WhatsApp, handles pairing, and writes message history to `messages.db`. It serves a small REST API: `/api/health`, `/api/status`, `/api/start-pairing`, `/api/pair-phone`, `/api/reconnect`, `/api/send`, `/api/download`, `/api/shutdown`. `GET /api/status` is read-only. Every state change is a `POST`.
2. **Python MCP server** (`whatsapp-mcp-server/`) implements the tools. It reads `messages.db` read-only and calls the bridge for sending, downloading and pairing.

The store directory defaults to `whatsapp-bridge/store`. It holds `messages.db`, `whatsapp.db` (the whatsmeow session), `bridge.token`, `bridge.lock`, `bridge.log` and downloaded media.

### Bridge lifecycle

The bridge is a shared daemon, one per store directory. Any MCP client starts it when it is absent. No client stops it, so it survives the client exiting and serves Claude Desktop and Claude Code at the same time. Its log is `<store>/bridge.log`.

- **Stopping it.** Set `WHATSAPP_BRIDGE_IDLE_EXIT` (for example `30m`) so it exits after that long without API activity. Only bridge calls count: send, download, pairing and `get_connection_status`. Or kill it: `curl -s http://127.0.0.1:8080/api/health` shows its `pid`, then run `kill <pid>`.
- **Restarting.** The next tool call that needs the bridge starts a new one. The MCP client does not need a restart. A failed start is retried 30 seconds later.
- **Stale reads after an idle exit.** The read tools never start the bridge, so they return history only up to the exit. Call `get_connection_status` first when you need fresh messages.
- **After you update the plugin or the repository,** restart the MCP server (for example, reconnect it under `/mcp` in Claude Code). On startup the server notices that the running bridge was built from older sources, builds the new version while the old bridge keeps serving, asks the old bridge to shut down (`POST /api/shutdown`), and starts the new one. The WhatsApp login is kept. Bridges older than 0.3.0 have no shutdown endpoint and are stopped with `SIGTERM` instead, after the server checks that the bridge accepts its token. Another open Claude session loses at most one call and then reuses the new bridge.
- **When it is not replaced automatically,** `get_connection_status` shows `restart_needed: true` with a hint. This happens while a pairing is in progress, when Go is not installed, for a prebuilt (release) binary, on Windows, and when the bridge stops in the middle of a session rather than at server startup. Kill the daemon as the hint says. The next bridge call rebuilds and starts the new version.
- **One bridge per WhatsApp login.** The bridge locks `<store>/bridge.lock` and exits with code 75 when another bridge holds it. The lock does not cover copies of a store. A second process that connects with the same login makes WhatsApp disconnect the first one, and the first reports `pairing: "replaced"`.

### Manual two-process mode

Set `WHATSAPP_BRIDGE_AUTOSTART=0` in the server's environment to stop it from building and starting the bridge. Then run the bridge yourself:

```bash
cd whatsapp-bridge
CGO_ENABLED=1 go run .      # or build it and run the binary
```

When stderr is a terminal, the bridge prints a pairing QR code right away. Otherwise pair with the tools. Use the same `WHATSAPP_STORE_DIR`, `WHATSAPP_BRIDGE_ADDR` and token on both sides. In this mode the server never builds or restarts the bridge and never reports `restart_needed`.

On Windows, enable cgo once with `go env -w CGO_ENABLED=1`.

## Configuration

No variable is required. The Go bridge and the Python server read the same names. When the server starts the bridge, it passes on `WHATSAPP_STORE_DIR`. The bridge inherits the rest of the environment.

| Variable | Default | Used by | Meaning |
| --- | --- | --- | --- |
| `WHATSAPP_STORE_DIR` | `whatsapp-bridge/store` (bridge alone: `store` in its working directory) | both | Directory with the databases, token, lock, log and downloaded media. |
| `WHATSAPP_API_BASE_URL` | `http://localhost:8080/api` | server | Where the server reaches the bridge. |
| `WHATSAPP_BRIDGE_ADDR` | derived from the API URL (`127.0.0.1:8080`) | both | Listen address of the bridge REST API. |
| `WHATSAPP_BRIDGE_TOKEN` | token file | both | API token. Overrides `<store>/bridge.token`. |
| `WHATSAPP_BRIDGE_IDLE_EXIT` | unset (never exits) | bridge | Go duration such as `30m` or `12h`. The bridge exits after this long without API activity. |
| `WHATSAPP_BRIDGE_AUTOSTART` | `1` | server | `0`, `false`, `no` or `off` disables the build and start (manual mode). |
| `WHATSAPP_BRIDGE_BIN` | `whatsapp-bridge/whatsapp-bridge` | server | Bridge binary to run, and where a build is written. |
| `WHATSAPP_BRIDGE_SRC` | `whatsapp-bridge` | server | Bridge source directory used for builds. |
| `WHATSAPP_BRIDGE_START_TIMEOUT` | `60` | server | Seconds to wait for a started bridge to answer. |
| `WHATSAPP_GO_BIN` | `go` from `PATH`, then common install paths | server | Go toolchain used to build the bridge. |
| `WHATSAPP_FFMPEG_BIN` | `ffmpeg` from `PATH`, then `/opt/homebrew/bin`, `/usr/local/bin`, `/usr/bin` | server | `ffmpeg` used to convert audio. |
| `WHATSAPP_DB_PATH` | `<store>/messages.db` | server | Path of `messages.db` for read queries. |
| `WHATSAPP_LOG_LEVEL` | `INFO` | server | Python log level. Logs go to stderr. |

## Security and privacy

- The REST API binds to `127.0.0.1`. It answers only requests whose `Host` is `localhost`, a loopback IP or the configured bind host, which blocks DNS rebinding.
- Requests with an `Origin` header are rejected, so no web page in a browser can call the API. `POST` bodies must be `application/json`. `GET` requests never change state.
- Every endpoint except `/api/health` requires the bearer token. See [The local bridge token](#the-local-bridge-token). A bridge started by hand without a token file has authentication disabled.
- Downloaded media is written below `<store>/<chat>/` only. Chat names and file names from senders are reduced to a safe bare name, so a sender cannot write outside the store.
- Message bodies are not logged.
- The store (`whatsapp-bridge/store/`), databases, token and logs are listed in `.gitignore`.

**Prompt injection.** A message you receive can contain instructions aimed at the model. An injected instruction could make the model call `send_file` on a sensitive local file (an SSH key, a `.env`) or send your messages to a chat. Nothing in this server prevents that. Keep per-tool approval enabled in your MCP client and review every `send_file` and `send_message` call before you approve it.

## Upgrading from the original (lharries) version

1. **Back up `whatsapp-bridge/store/`** (or your `WHATSAPP_STORE_DIR`) before you update. The new bridge migrates `whatsapp.db` on first run, and a stale bridge is replaced automatically when the MCP server starts. The migration is one-way: older versions cannot read the result.
2. **Stop the old bridge.** Only one bridge may run per WhatsApp login.
3. Register the MCP server as described in [Connect to Claude](#connect-to-claude). The old `go run main.go` workflow is replaced by the tools above.

The bridge repairs chats stored under a LID (`LID <digits>`) after connecting and after a history sync. A chat for which WhatsApp provides neither a name nor a phone number keeps the name `LID <digits>`.

## Troubleshooting

- **The client cannot find `uv`.** Use the absolute path from `which uv` as `command`.
- **`state: failed` or build errors.** `get_connection_status` includes `log_tail`. Check that Go 1.27 and a C compiler are installed. Set `WHATSAPP_GO_BIN` if the client cannot find Go. The full log is `<store>/bridge.log`. On Windows a missing C compiler shows as `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`
- **Stuck on `building`.** The first build takes 1-2 minutes. Later starts reuse the binary.
- **"port in use by another bridge/store".** A bridge for a different store uses the same port. Set `WHATSAPP_STORE_DIR` and `WHATSAPP_BRIDGE_ADDR` (with a matching `WHATSAPP_API_BASE_URL`) the same way in all clients.
- **"bridge running with unknown token".** The running bridge was started with a different token. Set `WHATSAPP_BRIDGE_TOKEN` to that token, or kill the bridge (pid from `/api/health`) and let the server start a new one.
- **`pairing: "replaced"`.** Another process uses the same login. Stop it, then call `get_connection_status` with `reconnect: true`. Without MCP: `curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $(cat <store>/bridge.token)" -d "{}" http://127.0.0.1:8080/api/reconnect`.
- **QR code expired.** Codes rotate about every 20 seconds. Call `get_pairing_qr` again or use `pair_with_phone_number`.
- **Device limit reached.** Remove a device under **Linked devices** in WhatsApp.
- **No messages after pairing.** History takes several minutes to arrive.
- **New messages missing.** The bridge may have exited after `WHATSAPP_BRIDGE_IDLE_EXIT`. Call `get_connection_status` to start it.
- **Out of sync.** Stop the bridge. Delete `messages.db*` and `whatsapp.db*` in the store directory, including the `-wal` and `-shm` files. This unlinks the device. Restart and pair again.

See also the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues) for Claude Desktop issues.

## Development

```bash
cd whatsapp-bridge && go test ./... && golangci-lint run
cd whatsapp-mcp-server && uv run pytest && uv run ruff check .
```

### Releases (maintainers)

Push a version tag. The Release workflow runs the tests, builds the bridge for macOS (arm64, amd64), Linux and Windows, and publishes the archives with `SHA256SUMS`:

```bash
git tag vX.Y.Z && git push origin vX.Y.Z
```

## License

MIT. Forked from [lharries/whatsapp-mcp](https://github.com/lharries/whatsapp-mcp).
