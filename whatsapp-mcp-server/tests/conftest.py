"""Shared fixtures: a temp messages.db with the bridge schema plus an optional whatsapp.db."""

import sqlite3
import sys
from pathlib import Path

import pytest

SERVER_DIR = Path(__file__).resolve().parent.parent
if str(SERVER_DIR) not in sys.path:
    sys.path.insert(0, str(SERVER_DIR))

SCHEMA = """
CREATE TABLE IF NOT EXISTS chats (
    jid TEXT PRIMARY KEY,
    name TEXT,
    last_message_time TIMESTAMP
);
CREATE TABLE IF NOT EXISTS messages (
    id TEXT,
    chat_jid TEXT,
    sender TEXT,
    content TEXT,
    timestamp TIMESTAMP,
    is_from_me BOOLEAN,
    media_type TEXT,
    filename TEXT,
    url TEXT,
    media_key BLOB,
    file_sha256 BLOB,
    file_enc_sha256 BLOB,
    file_length INTEGER,
    PRIMARY KEY (id, chat_jid),
    FOREIGN KEY (chat_jid) REFERENCES chats(jid)
);
"""

CHATS = [
    ("111@s.whatsapp.net", "Alice", "2025-01-01 10:02:00+00:00"),
    ("222@s.whatsapp.net", "Bob", "2025-01-01 09:00:00+00:00"),
    ("g1@g.us", "HY.group Support", "2025-01-02 08:00:00+00:00"),
    # Unmerged LID chat: only whatsmeow_lid_map knows its phone number.
    ("999000@lid", "Carol", "2025-01-03 08:00:00+00:00"),
]

# (id, chat_jid, sender, content, timestamp, is_from_me, media_type)
MESSAGES = [
    ("m1", "111@s.whatsapp.net", "111", "hello", "2025-01-01 10:00:00+00:00", 0, None),
    ("m2", "111@s.whatsapp.net", "111", "second", "2025-01-01 10:01:00+00:00", 0, None),
    ("m3", "111@s.whatsapp.net", "Me", "third", "2025-01-01 10:02:00+00:00", 1, "image"),
    ("m4", "222@s.whatsapp.net", "222", "bob says hi", "2025-01-01 09:00:00+00:00", 0, None),
    ("m5", "g1@g.us", "111", "group hello", "2025-01-02 08:00:00+00:00", 0, None),
    ("m6", "999000@lid", "999000", "from lid", "2025-01-03 08:00:00+00:00", 0, None),
]

LID_PN = ("999000", "15551234567")  # bare digits, as stored by whatsmeow


def make_messages_db(path: Path, wal: bool = True) -> None:
    conn = sqlite3.connect(path)
    if wal:
        conn.execute("PRAGMA journal_mode=WAL")
    conn.executescript(SCHEMA)
    conn.executemany("INSERT INTO chats VALUES (?, ?, ?)", CHATS)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) "
        "VALUES (?, ?, ?, ?, ?, ?, ?)",
        MESSAGES,
    )
    conn.commit()
    conn.close()


def make_whatsapp_db(path: Path) -> None:
    conn = sqlite3.connect(path)
    conn.execute("CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL)")
    conn.execute("INSERT INTO whatsmeow_lid_map VALUES (?, ?)", LID_PN)
    conn.commit()
    conn.close()


@pytest.fixture(autouse=True)
def _isolate_bridge_env(tmp_path_factory, monkeypatch):
    """Never touch a real store or bridge: manual mode, private store dir, fresh manager."""
    import bridge_manager

    monkeypatch.setenv("WHATSAPP_BRIDGE_AUTOSTART", "0")
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path_factory.mktemp("store")))
    for var in ("WHATSAPP_BRIDGE_ADDR", "WHATSAPP_BRIDGE_BIN", "WHATSAPP_BRIDGE_SRC", "WHATSAPP_GO_BIN"):
        monkeypatch.delenv(var, raising=False)
    bridge_manager.reset_manager()
    yield
    bridge_manager.reset_manager()


@pytest.fixture
def store_dir(tmp_path, monkeypatch):
    """Temp store dir with messages.db (WAL) and whatsapp.db; env points the server at it."""
    make_messages_db(tmp_path / "messages.db")
    make_whatsapp_db(tmp_path / "whatsapp.db")
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(tmp_path / "messages.db"))
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path))
    monkeypatch.delenv("WHATSAPP_API_BASE_URL", raising=False)
    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    return tmp_path
