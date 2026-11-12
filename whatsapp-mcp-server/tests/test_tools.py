"""Every MCP tool invoked through FastMCP (exercises output-schema validation), plus DB behaviour."""

import json
import shutil
import sqlite3
from pathlib import Path

import httpx
import pytest
from mcp.server.fastmcp.exceptions import ToolError

import main
import whatsapp
from conftest import make_messages_db

BASELINE = Path(__file__).parent / "baseline_tools.json"


def _payload(result):
    """Return the structured payload of a call_tool result (unwrapping FastMCP's {"result": ...})."""
    content, structured = result
    if structured is not None:
        return structured.get("result", structured) if set(structured) == {"result"} else structured
    return json.loads(content[0].text)


async def call(name, **args):
    return _payload(await main.mcp.call_tool(name, args))


@pytest.fixture
def bridge_ok(monkeypatch):
    def handler(request):
        if request.url.path.endswith("/download"):
            return httpx.Response(200, json={"success": True, "path": "/store/x.jpg"})
        return httpx.Response(200, json={"success": True, "message": "sent"})

    monkeypatch.setattr(whatsapp, "_http_transport", httpx.MockTransport(handler))


# --- tools -------------------------------------------------------------------------------------


async def test_search_contacts(store_dir):
    res = await call("search_contacts", query="Alice")
    assert res == [{"phone_number": "111", "name": "Alice", "jid": "111@s.whatsapp.net"}]
    assert await call("search_contacts", query="zzz-none") == []


async def test_list_messages(store_dir):
    out = await call("list_messages", chat_jid="111@s.whatsapp.net", include_context=False)
    assert isinstance(out, str) and "hello" in out and "second" in out
    assert "[image - Message ID: m3" in out
    assert await call("list_messages", query="no-such-text") == "No messages to display."


async def test_list_messages_dates_and_context(store_dir):
    out = await call("list_messages", after="2025-01-01T10:00:30", before="2025-01-01T10:01:30",
                     chat_jid="111@s.whatsapp.net", include_context=False)
    assert "second" in out and "hello" not in out and "third" not in out
    with_ctx = await call("list_messages", query="second", context_before=1, context_after=1)
    assert "hello" in with_ctx and "third" in with_ctx
    with pytest.raises(ToolError):
        await call("list_messages", after="garbage")


async def test_list_chats(store_dir):
    res = await call("list_chats")
    assert [c["jid"] for c in res][0] == "999000@lid"
    alice = next(c for c in res if c["jid"] == "111@s.whatsapp.net")
    assert alice["last_message"] == "third"
    assert alice["last_message_time"] == "2025-01-01T10:02:00+00:00"
    assert [c["name"] for c in await call("list_chats", sort_by="name", include_last_message=False)][0] == "Alice"


async def test_get_chat(store_dir):
    chat = await call("get_chat", chat_jid="222@s.whatsapp.net")
    assert chat["name"] == "Bob" and chat["last_message"] == "bob says hi"
    assert await call("get_chat", chat_jid="nope@s.whatsapp.net") is None


async def test_get_direct_chat_by_contact(store_dir):
    chat = await call("get_direct_chat_by_contact", sender_phone_number="222")
    assert chat["jid"] == "222@s.whatsapp.net"
    assert await call("get_direct_chat_by_contact", sender_phone_number="000000") is None


async def test_get_contact_chats(store_dir):
    res = await call("get_contact_chats", jid="111")
    assert {c["jid"] for c in res} >= {"111@s.whatsapp.net", "g1@g.us"}
    assert await call("get_contact_chats", jid="nobody") == []


async def test_get_last_interaction(store_dir):
    out = await call("get_last_interaction", jid="111@s.whatsapp.net")
    assert "third" in out
    assert await call("get_last_interaction", jid="nobody@s.whatsapp.net") is None


async def test_get_message_context(store_dir):
    ctx = await call("get_message_context", message_id="m2", before=1, after=1)
    assert ctx["message"]["id"] == "m2"
    assert ctx["message"]["timestamp"] == "2025-01-01T10:01:00+00:00"
    assert [m["id"] for m in ctx["before"]] == ["m1"]
    assert [m["id"] for m in ctx["after"]] == ["m3"]
    with pytest.raises(ToolError, match="not found"):
        await call("get_message_context", message_id="missing")


async def test_send_message(store_dir, bridge_ok):
    assert await call("send_message", recipient="123", message="hi") == {"success": True, "message": "sent"}
    res = await call("send_message", recipient="", message="hi")
    assert res == {"success": False, "message": "Recipient must be provided"}


async def test_send_file_and_audio(store_dir, bridge_ok, tmp_path):
    f = tmp_path / "a.ogg"
    f.write_bytes(b"OggS")
    assert (await call("send_file", recipient="123", media_path=str(f)))["success"] is True
    assert (await call("send_audio_message", recipient="123", media_path=str(f)))["success"] is True
    res = await call("send_file", recipient="123", media_path=str(tmp_path / "missing"))
    assert res["success"] is False and "not found" in res["message"]
    res = await call("send_audio_message", recipient="123", media_path=str(tmp_path / "missing"))
    assert res["success"] is False


async def test_download_media(store_dir, bridge_ok, monkeypatch):
    res = await call("download_media", message_id="m3", chat_jid="111@s.whatsapp.net")
    assert res == {"success": True, "message": "Media downloaded successfully", "file_path": "/store/x.jpg"}
    monkeypatch.setattr(whatsapp, "_http_transport", httpx.MockTransport(
        lambda r: httpx.Response(200, json={"success": False, "message": "no media"})))
    res = await call("download_media", message_id="m1", chat_jid="x")
    assert res == {"success": False, "message": "Failed to download media"}


# --- search / LID ------------------------------------------------------------------------------


async def test_list_chats_name_with_dot(store_dir):
    res = await call("list_chats", query="HY.group")
    assert [c["name"] for c in res] == ["HY.group Support"]


async def test_phone_query_finds_unmerged_lid_chat(store_dir):
    res = await call("list_chats", query="15551234567")
    assert [c["jid"] for c in res] == ["999000@lid"]
    contacts = await call("search_contacts", query="5551234")
    assert [c["jid"] for c in contacts] == ["999000@lid"]


async def test_missing_whatsapp_db_is_silent(store_dir):
    (store_dir / "whatsapp.db").unlink()
    assert [c["jid"] for c in await call("list_chats", query="15551234567")] == []
    assert [c["name"] for c in await call("list_chats", query="Alice")] == ["Alice"]
    assert [c["name"] for c in await call("search_contacts", query="Bob")] == ["Bob"]


async def test_whatsapp_db_without_lid_table(store_dir):
    (store_dir / "whatsapp.db").unlink()
    sqlite3.connect(store_dir / "whatsapp.db").close()  # empty DB, no whatsmeow_lid_map
    assert [c["name"] for c in await call("list_chats", query="Alice")] == ["Alice"]


def test_wal_db_readable_after_writer_closed(tmp_path, monkeypatch):
    db = tmp_path / "messages.db"
    make_messages_db(db, wal=True)
    for suffix in ("-wal", "-shm"):
        assert not (tmp_path / f"messages.db{suffix}").exists()
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(db))
    with whatsapp._connect() as conn:
        assert conn.execute("SELECT COUNT(*) FROM chats").fetchone()[0] == 4
        assert conn.execute("PRAGMA journal_mode").fetchone()[0] == "wal"


def test_connect_is_read_only(store_dir):
    with whatsapp._connect() as conn:
        with pytest.raises(sqlite3.OperationalError):
            conn.execute("DELETE FROM chats")


# --- tool schema baseline ----------------------------------------------------------------------


def _normalize(schema: dict) -> dict:
    """Reduce an input schema to param names, required list and base types."""
    def base_type(prop):
        if "type" in prop:
            return prop["type"]
        types = sorted(t["type"] for t in prop.get("anyOf", []) if t.get("type") != "null")
        return types[0] if len(types) == 1 else types

    return {
        "params": {name: base_type(p) for name, p in schema.get("properties", {}).items()},
        "required": sorted(schema.get("required", [])),
    }


async def test_tool_schemas_match_baseline():
    baseline = json.loads(BASELINE.read_text())
    tools = {t.name: t.inputSchema for t in await main.mcp.list_tools()}
    # The baseline tools stay unchanged; Task 9a adds exactly these pairing/status tools.
    assert set(tools) == set(baseline) | {"get_connection_status", "get_pairing_qr", "pair_with_phone_number"}
    for name, schema in baseline.items():
        assert _normalize(tools[name]) == _normalize(schema), name
