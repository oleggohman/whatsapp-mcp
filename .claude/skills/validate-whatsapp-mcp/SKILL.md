---
name: validate-whatsapp-mcp
description: Use when asked to validate or test the WhatsApp MCP tools.
---

# Validate WhatsApp MCP

## Overview

Exercise all 16 `mcp__plugin_whatsapp_whatsapp__*` tools against the live account and report one row per tool.
A tool passes only when its output is checked, not when it merely returns `success: true`.
Read-only tools run freely. Send tools need the user's explicit OK and go only to the user's own "Message yourself" chat.

Paths used below:
- Store: `~/.claude/plugins/data/whatsapp-oleggohman-cc-things/store` (`messages.db`, `whatsapp.db`, `bridge.log`, `bridge.token`)
- Installed plugin: `~/.claude/plugins/installed_plugins.json`, key `whatsapp@oleggohman-cc-things`
- Marketplace: `~/.claude/plugins/marketplaces/oleggohman-cc-things/.claude-plugin/marketplace.json`

## Steps

1. **Version.** Compare installed version and commit, marketplace version and ref, and the newest repo tag:
   ```bash
   jq -c '.plugins["whatsapp@oleggohman-cc-things"][0] | {version,gitCommitSha,lastUpdated}' ~/.claude/plugins/installed_plugins.json
   jq -c '.plugins[] | select(.name=="whatsapp") | {version,ref:.source.ref}' ~/.claude/plugins/marketplaces/oleggohman-cc-things/.claude-plugin/marketplace.json
   git fetch -q --tags   # needs github.com network access; local tags lag otherwise
   git tag --sort=-v:refname | head -1
   ```
2. **Connection.** `get_connection_status`: expect `state: running`, `connected: true`, `logged_in: true`, `pairing: idle`. If `restart_needed` is true, the running bridge is older than the sources. Report it. If not logged in, stop and tell the user to pair.
3. **Read tools.** Chain them so every ID comes from live data, never hardcoded:
   - `list_chats` (default sort, then `sort_by: "name"` with a `query`), then `get_chat` on one JID from it.
   - `list_messages` (`limit` 3-5, `include_context: false`), then `get_message_context` on a message ID from it.
   - `search_contacts` on a short query, then `get_last_interaction`, `get_contact_chats` and `get_direct_chat_by_contact` on one direct contact (a `@s.whatsapp.net` JID) from it.
   - Check that the tools agree: same chat name, same last message, same sender.
4. **Pairing tools.** Run only when step 2 shows `logged_in: true`. Then `get_pairing_qr` must say it is already paired, and `pair_with_phone_number` (with dummy `14155552671`) must say "already logged in". When logged out, these start a real pairing, so skip them.
5. **`download_media`.** Find a recent image and a recent audio or document with `list_messages`, download each, then check the files really are media:
   ```bash
   file ~/.claude/plugins/data/whatsapp-oleggohman-cc-things/store/<chat_jid>/<file>
   ```
   Since 0.4.2 a failure returns the bridge's real error in `message` (signed URL queries redacted). If you need more, ask the bridge directly:
   ```bash
   S=~/.claude/plugins/data/whatsapp-oleggohman-cc-things/store
   curl -s -m 60 -X POST -H "Authorization: Bearer $(cat $S/bridge.token)" -H 'Content-Type: application/json' \
     -d '{"message_id":"<id>","chat_jid":"<jid>"}' http://127.0.0.1:8080/api/download
   grep -n "<id>" $S/bridge.log | tail
   ```
   Root-cause from there (read `whatsapp-bridge/media.go`) before calling anything a bug.
6. **Send tools: ask first.** Ask the user: "Send 3 test messages to your own 'Message yourself' chat?" Without a clear yes, mark the send tools "not tested". With a yes:
   - Get the user's own number at run time. Never write it into files or memory:
     ```bash
     sqlite3 "file:$HOME/.claude/plugins/data/whatsapp-oleggohman-cc-things/store/whatsapp.db?mode=ro" \
       "SELECT substr(jid,1,instr(jid,':')-1) FROM whatsmeow_device;"
     ```
   - Make the test files in the session scratchpad (`D` = scratchpad dir):
     ```bash
     D="<scratchpad dir>"
     echo "whatsapp-mcp test 2/3 (send_file)" > "$D/wa-test.txt"
     ffmpeg -loglevel error -y -f lavfi -i "sine=frequency=440:duration=2" "$D/wa-test.mp3"
     ```
   - Call `send_message` ("whatsapp-mcp test 1/3"), `send_file` (the .txt) and `send_audio_message` (the .mp3), each to that number only.
   - Check that all three arrived with `list_messages` on `<own number>@s.whatsapp.net`. Then `download_media` the sent document and compare its contents and file name with the original.
   - Delete the scratchpad test files.
7. **Report.** Give a table: tool, result (✅ / ❌ / ⏸ not tested), evidence. For each ❌, give the cause with `file:line` and the proposed fix. Change no code unless the user asks.

## Known non-bugs

| What you see | Why |
|---|---|
| Sender or chat name like `+972∙∙∙∙∙∙∙34` | WhatsApp's masked number, stored as the name. Expected only when neither WhatsApp nor macOS Contacts knows a name. If `search_contacts` finds the number by a Contacts name but still shows the masked form, that is a bug (masked names must count as placeholders). |
| A name WhatsApp never had (from macOS Contacts) | Since 0.4.2 the MCP falls back to macOS Contacts by phone number. Off with `WHATSAPP_MACOS_CONTACTS=0`. Group senders get a Contacts name only when a direct chat with them exists. |
| Newsletter chats named `+1203632...` | Channels have no contact name. Cosmetic. |
| `file_path` starting with a literal `$HOME/` | Path redaction in the tool output, not in MCP code. The file exists at the expanded path. |
| `get_pairing_qr` refusing | Expected while logged in. |

## Common mistakes

- Reporting "works" after read tools only. All 16 tools get a row, even if the row says "not tested" and why.
- Trusting `success: true`. Run `file` on downloads and read sent messages back.
- Reporting a download failure without the bridge's real error from step 5.
- Sending to any chat other than the user's own, or sending without asking first.
- Hardcoding message IDs, JIDs or phone numbers in this skill or in memory.
