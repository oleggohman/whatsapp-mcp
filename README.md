# WhatsApp MCP Server

This is a Model Context Protocol (MCP) server for WhatsApp.

With this you can search and read your personal Whatsapp messages (including images, videos, documents, and audio messages), search your contacts and send messages to either individuals or groups. You can also send media files including images, videos, documents, and audio messages.

It connects to your **personal WhatsApp account** directly via the Whatsapp web multidevice API (using the [whatsmeow](https://github.com/tulir/whatsmeow) library). All your messages are stored locally in a SQLite database and only sent to an LLM (such as Claude) when the agent accesses them through tools (which you control).

Here's an example of what you can do when it's connected to Claude.

![WhatsApp MCP](./example-use.png)

> To get updates on this and other projects I work on [enter your email here](https://docs.google.com/forms/d/1rTF9wMBTN0vPfzWuQa2BjfGKdKIpTbyeKxhPMcEzgyI/preview)

> *Caution:* as with many MCP servers, the WhatsApp MCP is subject to [the lethal trifecta](https://simonwillison.net/2025/Jun/16/the-lethal-trifecta/). This means that prompt injection could lead to private data exfiltration. See [Security model](#security-model).

## Installation

You configure one MCP server (`whatsapp-mcp-server`). It builds, starts and supervises the Go bridge for you; you do not run the bridge by hand.

### Prerequisites

- Go 1.27 and cgo with a C compiler (`go-sqlite3` needs cgo). On macOS install the Xcode command line tools, on Linux `gcc`, on Windows [MSYS2](https://www.msys2.org/) with `ucrt64\bin` on your `PATH`.
- Python 3.11+
- [uv](https://docs.astral.sh/uv/) (Python package manager), install with `curl -LsSf https://astral.sh/uv/install.sh | sh`
- FFmpeg (_optional_) - only needed for voice messages. Voice messages must be `.ogg` Opus; with FFmpeg the server converts other audio formats automatically. Without it, send raw audio with `send_file`.

### Setup

1. **Clone this repository**

   ```bash
   git clone https://github.com/lharries/whatsapp-mcp.git
   cd whatsapp-mcp
   ```

2. **Register the MCP server**

   Run `which uv` and `pwd` (inside the repo) and substitute the absolute paths.

   **Claude Desktop** - edit `claude_desktop_config.json` (macOS: `~/Library/Application Support/Claude/claude_desktop_config.json`):

   ```json
   {
     "mcpServers": {
       "whatsapp": {
         "command": "/absolute/path/to/uv",
         "args": [
           "--directory",
           "/absolute/path/to/whatsapp-mcp/whatsapp-mcp-server",
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

   The `env` block is optional. No variable is required: the server inherits `HOME` from the client, which is all `go build` and `uv` need. Add `WHATSAPP_GO_BIN` (absolute path to the `go` binary) when the GUI app starts with a minimal `PATH` that does not contain Go. The server also probes `/opt/homebrew/bin`, `/usr/local/go/bin`, `/usr/local/bin`, `~/go/bin` and `~/sdk/*/bin` on its own. If your client strips `HOME`, set `HOME` or `GOCACHE` in `env` as well. The same minimal `PATH` affects `ffmpeg` (used by `send_audio_message` to convert non-`.ogg` audio): the server looks for it on `PATH`, then in `/opt/homebrew/bin`, `/usr/local/bin` and `/usr/bin`; set `WHATSAPP_FFMPEG_BIN` (absolute path) if it lives elsewhere.

   **Claude Code**:

   ```bash
   claude mcp add whatsapp -- uv --directory /absolute/path/to/whatsapp-mcp/whatsapp-mcp-server run main.py
   # with environment variables:
   claude mcp add whatsapp -e WHATSAPP_GO_BIN=/usr/local/go/bin/go -- uv --directory /absolute/path/to/whatsapp-mcp/whatsapp-mcp-server run main.py
   ```

   **Cursor** - use the same `mcpServers` block in `~/.cursor/mcp.json`.

3. **Restart the client.** The first start builds the bridge, which takes about 1-2 minutes; `get_connection_status` reports `building` meanwhile. The bridge is rebuilt automatically whenever its sources are newer than the binary.

### Pairing

Ask the assistant to pair your account; it uses these tools:

- `get_pairing_qr` returns a QR code image. Scan it in WhatsApp > Linked devices > Link a device. The QR code rotates about every 20 seconds; call the tool again if the scan fails.
- `pair_with_phone_number` takes your number (country code, digits only) and returns a code. Enter it in WhatsApp > Linked devices > Link a device > Link with phone number instead.
- `get_connection_status` shows `state`, `connected`, `logged_in` and `pairing` (`idle`, `qr`, `code`, `replaced`). With `reconnect: true` it first takes the login back after `replaced`.

Message history loads for several minutes after pairing. WhatsApp may ask you to re-authenticate after about 20 days.

## Bridge lifecycle

The bridge is a shared daemon, one per store directory. Any MCP client spawns it when it is absent, and none of them stops it: it survives the client exiting and is shared by Claude Desktop and Claude Code at the same time. Its log is `<store>/bridge.log`. When the supervisor starts a bridge and the log is over 5 MB, it moves it to `bridge.log.1` first; a running bridge keeps appending to its log until it exits, so a long-lived daemon can grow it past 5 MB (message bodies are never logged).

- **Stop it** by setting `WHATSAPP_BRIDGE_IDLE_EXIT` (for example `30m`) so it exits after that long without API activity, or by killing it: `curl -s http://127.0.0.1:8080/api/health` shows its `pid`, then `kill <pid>`. The next tool call that needs the bridge (send, download, pairing, `get_connection_status`) spawns a new one; the MCP client does not need a restart. A failed start (for example Go missing or the port taken) is retried the same way 30 seconds after the failure.
- **After updating the repository**, the supervisor rebuilds the binary but does not replace a running daemon. `get_connection_status` then shows `restart_needed: true`; kill the daemon as above.
- **One bridge per WhatsApp login.** The bridge takes a lock on `<store>/bridge.lock` and exits with code 75 when another bridge already holds it for the same store directory (the supervisor treats that as "a peer is starting"). The lock does not protect *copies* of a store: if a second process connects with the same WhatsApp device, WhatsApp kicks the first one off. The bridge then reports `pairing: "replaced"` and does **not** reconnect on its own (two bridges would kick each other forever); stop the other process, then call `get_connection_status` with `reconnect: true` to take the login back (the tool sends `POST /api/reconnect`).

## Environment variables

Bridge (Go) and MCP server (Python) read the same names. When the server spawns the bridge it passes `WHATSAPP_STORE_DIR` and `WHATSAPP_BRIDGE_ADDR` on, and the bridge inherits the rest.

| Variable | Default | Used by | Meaning |
| --- | --- | --- | --- |
| `WHATSAPP_STORE_DIR` | `whatsapp-bridge/store` (the bridge alone: `store` in its working directory) | both | Directory with `messages.db`, `whatsapp.db`, `bridge.lock`, `bridge.token`, `bridge.log` and downloaded media |
| `WHATSAPP_API_BASE_URL` | `http://localhost:8080/api` | server | Where the server reaches the bridge |
| `WHATSAPP_BRIDGE_ADDR` | derived from the API URL (`127.0.0.1:8080`) | both | Listen address of the bridge REST API; loopback by default |
| `WHATSAPP_BRIDGE_TOKEN` | token file | both | API token; overrides `<store>/bridge.token` |
| `WHATSAPP_BRIDGE_IDLE_EXIT` | unset (never) | bridge | Go duration (`30m`, `12h`); the bridge exits after this long without API activity (health polls do not count) |
| `WHATSAPP_BRIDGE_AUTOSTART` | `1` | server | `0`/`false`/`no`/`off` disables build and spawn (manual mode) |
| `WHATSAPP_BRIDGE_BIN` | `whatsapp-bridge/whatsapp-bridge` | server | Bridge binary to run (and where the build is written) |
| `WHATSAPP_BRIDGE_SRC` | `whatsapp-bridge` | server | Bridge source directory used for builds |
| `WHATSAPP_BRIDGE_START_TIMEOUT` | `60` | server | Seconds to wait for the spawned bridge to answer |
| `WHATSAPP_GO_BIN` | `go` from `PATH`, then common install paths | server | Go toolchain used to build the bridge |
| `WHATSAPP_FFMPEG_BIN` | `ffmpeg` from `PATH`, then common install paths | server | ffmpeg used by `send_audio_message` to convert audio |
| `WHATSAPP_DB_PATH` | `<store>/messages.db` | server | Override the `messages.db` path for read queries |
| `WHATSAPP_LOG_LEVEL` | `INFO` | server | Python log level (logs go to stderr) |

## Security model

- The REST API binds to `127.0.0.1` and only answers requests whose `Host` is localhost, a loopback IP or the configured bind host (DNS-rebinding defence). Requests carrying an `Origin` header (any browser) are rejected, and `POST` bodies must be `application/json`. `GET` requests never change state (pairing and reconnect are `POST` only), so a cross-site link or image cannot trigger them.
- Every endpoint except `/api/health` requires a bearer token. The MCP server generates it on first start as `<store>/bridge.token` (mode `0600`, created atomically) and all clients of the same store share it. `WHATSAPP_BRIDGE_TOKEN` overrides the file. A bridge started by hand without a token file has authentication disabled.
- Downloaded media is written below `<store>/<chat>/` only; chat names and the remote-controlled file names are reduced to a bare, safe name so a sender cannot write outside the store.
- Message bodies are not logged.

**Limitation:** none of this stops a prompt-injected model from calling `send_file` on a sensitive local file (for example an SSH key or a `.env`) and sending it to a chat. Rely on your MCP client's per-tool approval and review `send_file` and `send_message` calls before approving them.

## Upgrading from the old version

- **Back up `store/` first** (`whatsapp-bridge/store/`, or your `WHATSAPP_STORE_DIR`). The updated bridge migrates the whatsmeow database `whatsapp.db` on first run, and that migration is one-way: older bridge versions cannot read it afterwards.
- The old workflow (`go run main.go` in a terminal, QR code in the console) is replaced by the tools above. You can still run the bridge in a terminal; see [manual mode](#manual-two-process-mode-advanced).
- Chats that were stored under an opaque LID (`LID <digits>`) are repaired automatically by the bridge after a history sync and on every connect.
- **LID limitation:** a chat for which WhatsApp provides no name or phone-number information keeps the name `LID <digits>`.

## Manual two-process mode (advanced)

Set `WHATSAPP_BRIDGE_AUTOSTART=0` in the MCP server's environment to stop it from building and spawning the bridge. You then run the bridge yourself:

```bash
cd whatsapp-bridge
CGO_ENABLED=1 go run .      # or build it and run the binary
```

When stderr is a terminal the bridge prints a pairing QR code right away; otherwise pair through the tools. Use the same `WHATSAPP_STORE_DIR`, `WHATSAPP_BRIDGE_ADDR` and token on both sides. In this mode the server never builds, restarts or reports `restart_needed`.

On Windows enable cgo once with `go env -w CGO_ENABLED=1` (a missing C compiler shows up as `Binary was compiled with 'CGO_ENABLED=0', go-sqlite3 requires cgo to work.`).

## Architecture Overview

1. **Go WhatsApp Bridge** (`whatsapp-bridge/`): connects to the WhatsApp web multidevice API, handles pairing, and keeps message history in SQLite (`messages.db`) in the store directory. It serves a small authenticated REST API (`/api/health`, `/api/status`, `/api/start-pairing`, `/api/reconnect`, `/api/pair-phone`, `/api/send`, `/api/download`). `GET /api/status` is read-only; every state change is a `POST`.
2. **Python MCP Server** (`whatsapp-mcp-server/`): implements the MCP tools. It reads `messages.db` read-only, calls the bridge for sending, downloading and pairing, and supervises the bridge process.

## Usage

Once connected, you can interact with your WhatsApp contacts through Claude.

### MCP Tools

Claude can access the following tools to interact with WhatsApp:

- **search_contacts**: Search for contacts by name or phone number
- **list_messages**: Retrieve messages with optional filters and context
- **list_chats**: List available chats with metadata
- **get_chat**: Get information about a specific chat
- **get_direct_chat_by_contact**: Find a direct chat with a specific contact
- **get_contact_chats**: List all chats involving a specific contact
- **get_last_interaction**: Get the most recent message with a contact
- **get_message_context**: Retrieve context around a specific message
- **send_message**: Send a WhatsApp message to a specified phone number or group JID
- **send_file**: Send a file (image, video, raw audio, document) to a specified recipient
- **send_audio_message**: Send an audio file as a WhatsApp voice message (requires the file to be an .ogg opus file or ffmpeg must be installed)
- **download_media**: Download media from a WhatsApp message and get the local file path
- **get_connection_status**: Bridge state (`building`, `starting`, `running`, `failed`) plus `connected`, `logged_in` and `pairing`
- **get_pairing_qr**: Get a QR code image to link this server to a WhatsApp account
- **pair_with_phone_number**: Get a pairing code to link with a phone number instead of a QR code

### Media Handling Features

The MCP server supports both sending and receiving various media types:

#### Media Sending

You can send various media types to your WhatsApp contacts:

- **Images, Videos, Documents**: Use the `send_file` tool to share any supported media type.
- **Voice Messages**: Use the `send_audio_message` tool to send audio files as playable WhatsApp voice messages.
  - For optimal compatibility, audio files should be in `.ogg` Opus format.
  - With FFmpeg installed, the system will automatically convert other audio formats (MP3, WAV, etc.) to the required format.
  - Without FFmpeg, you can still send raw audio files using the `send_file` tool, but they won't appear as playable voice messages.

#### Media Downloading

By default, just the metadata of the media is stored in the local database. The message will indicate that media was sent. To access this media you need to use the download_media tool which takes the `message_id` and `chat_jid` (which are shown when printing messages containing the meda), this downloads the media and then returns the file path which can be then opened or passed to another tool.

## Development

```bash
cd whatsapp-bridge && go test ./... && golangci-lint run
cd whatsapp-mcp-server && uv run pytest && uv run ruff check .
```

## Troubleshooting

- **uv not found by the client**: use the absolute path from `which uv` as `command`.
- **`state: failed` / build errors**: `get_connection_status` includes `log_tail`. Check that Go 1.27 and a C compiler are installed; set `WHATSAPP_GO_BIN` if the client cannot find Go. Full bridge log: `<store>/bridge.log`.
- **Stuck on `building`**: the first build takes 1-2 minutes; later starts reuse the binary.
- **"port in use by another bridge/store"**: another bridge serves a different store on the same port. Set `WHATSAPP_STORE_DIR` and `WHATSAPP_BRIDGE_ADDR` (with a matching `WHATSAPP_API_BASE_URL`) consistently for all clients.
- **"bridge running with unknown token"**: the running bridge was started with a different token. Set `WHATSAPP_BRIDGE_TOKEN` to it, or kill the bridge (pid from `/api/health`) and let the server restart it.
- **`pairing: "replaced"`**: another process connected with the same WhatsApp login. Stop the other one, then call `get_connection_status` with `reconnect: true` (without MCP: `curl -s -X POST -H "Content-Type: application/json" -H "Authorization: Bearer $(cat <store>/bridge.token)" -d "{}" http://127.0.0.1:8080/api/reconnect`).
- **QR code expired**: codes rotate about every 20 seconds; call `get_pairing_qr` again, or use `pair_with_phone_number`.
- **Device limit reached**: remove a device in WhatsApp > Linked devices.
- **No messages loading**: after pairing, history can take several minutes to arrive.
- **Out of sync**: stop the bridge, delete `messages.db` and `whatsapp.db` in the store directory (this unlinks the device), restart and pair again.

For additional Claude Desktop integration troubleshooting, see the [MCP documentation](https://modelcontextprotocol.io/quickstart/server#claude-for-desktop-integration-issues).
