"""Shared fixtures: a temp messages.db with the bridge schema plus an optional whatsapp.db."""

import plistlib
import sqlite3
import sys
from pathlib import Path

import pytest

SERVER_DIR = Path(__file__).resolve().parent.parent
if str(SERVER_DIR) not in sys.path:
    sys.path.insert(0, str(SERVER_DIR))

import contacts_macos  # noqa: E402

DEFAULT_STORE = SERVER_DIR.parent / "whatsapp-bridge" / "store"


@pytest.fixture(autouse=True)
def _isolate_store(tmp_path_factory, monkeypatch):
    """Never let a test fall back to the real default store (it may hold a live login).

    Every test gets a private store dir and autostart off; fixtures that need
    something else override these with their own monkeypatch.setenv calls.
    """
    store = tmp_path_factory.mktemp("isolated-store")
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(store))
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(store / "messages.db"))
    monkeypatch.setenv("WHATSAPP_BRIDGE_AUTOSTART", "0")
    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    # Never read the real AddressBook; Contacts tests opt in with their own fake tree.
    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", "0")
    contacts_macos.reset()
    yield
    contacts_macos.reset()
    assert not (DEFAULT_STORE / "bridge.token").exists(), (
        f"test wrote into the real default store {DEFAULT_STORE}"
    )


def make_addressbook(
    root: Path,
    sources: dict[str, list[tuple[str, str, str, list[str]]]],
    default: str | None = None,
) -> Path:
    """Write a fake AddressBook tree under root and return the path of its prefs plist.

    sources maps a source key to records (first, last, organization, numbers); key "" is the
    root database, any other key is root/Sources/<key>/. A binary plist with
    ABDefaultSourceID is written to root/prefs.plist when default is given.
    """
    for key, records in sources.items():
        folder = root / "Sources" / key if key else root
        folder.mkdir(parents=True, exist_ok=True)
        conn = sqlite3.connect(folder / "AddressBook-v22.abcddb")
        conn.execute("PRAGMA journal_mode=WAL")
        conn.execute(
            "CREATE TABLE ZABCDRECORD (Z_PK INTEGER PRIMARY KEY, "
            "ZFIRSTNAME TEXT, ZLASTNAME TEXT, ZORGANIZATION TEXT)"
        )
        conn.execute(
            "CREATE TABLE ZABCDPHONENUMBER (Z_PK INTEGER PRIMARY KEY, ZOWNER INTEGER, "
            "ZFULLNUMBER TEXT)"
        )
        for pk, (first, last, org, numbers) in enumerate(records, start=1):
            conn.execute("INSERT INTO ZABCDRECORD VALUES (?, ?, ?, ?)", (pk, first, last, org))
            for number in numbers:
                conn.execute(
                    "INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (?, ?)",
                    (pk, number),
                )
        conn.commit()
        conn.close()
    prefs = root / "prefs.plist"
    if default is not None:
        prefs.write_bytes(plistlib.dumps({"ABDefaultSourceID": default}, fmt=plistlib.FMT_BINARY))
    return prefs


# The index, FTS and trigger DDL below copies migrateIndexesSQL, migrateFTSTableSQL and
# migrateFTSTriggersSQL in whatsapp-bridge/store.go; test_schema_sync.py fails if they drift.
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
CREATE INDEX IF NOT EXISTS idx_messages_chat_time ON messages (chat_jid, timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_timestamp ON messages (timestamp);
CREATE INDEX IF NOT EXISTS idx_messages_sender_time ON messages (sender, timestamp);
CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content='messages',
    content_rowid='rowid',
    tokenize='trigram case_sensitive 0'
);
CREATE TRIGGER IF NOT EXISTS messages_fts_insert AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_delete AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
    VALUES ('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_fts_update AFTER UPDATE OF content ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content)
    VALUES ('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
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
    # Rebuild so rows inserted above are searchable regardless of trigger state.
    conn.execute("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')")
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
    for var in (
        "WHATSAPP_BRIDGE_ADDR",
        "WHATSAPP_BRIDGE_BIN",
        "WHATSAPP_BRIDGE_SRC",
        "WHATSAPP_GO_BIN",
    ):
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


def enable_contacts(root: Path, monkeypatch, sources, default: str | None = None) -> None:
    """Point contacts_macos at a fake AddressBook tree under root, as if running on macOS."""
    prefs = make_addressbook(root, sources, default)
    monkeypatch.setattr(contacts_macos, "_is_macos", lambda: True)
    monkeypatch.setattr(contacts_macos, "addressbook_dir", lambda: root)
    monkeypatch.setattr(contacts_macos, "preferences_path", lambda: prefs)
    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", "1")
    contacts_macos.reset()


# Placeholder-named direct chat (Mia), a LID chat whose digits also sit in Contacts, and a
# chat whose number matches Mia's national digits only behind a 5 digit prefix.
CONTACTS_CHATS = [
    ("972585529067@s.whatsapp.net", "+972585529067", "2025-01-04 08:00:00+00:00"),
    ("1999000111@lid", "LID 1999000111", "2025-01-05 08:00:00+00:00"),
    ("12345585529067@s.whatsapp.net", "Stranger", "2025-01-06 08:00:00+00:00"),
]
# (id, chat_jid, sender, content, timestamp, is_from_me)
CONTACTS_MESSAGES = [
    (
        "p1",
        "972585529067@s.whatsapp.net",
        "972585529067",
        "mia one",
        "2025-01-04 08:00:00+00:00",
        0,
    ),
    (
        "p2",
        "972585529067@s.whatsapp.net",
        "972585529067",
        "mia two",
        "2025-01-04 08:01:00+00:00",
        0,
    ),
    ("p3", "972585529067@s.whatsapp.net", "Me", "mia three", "2025-01-04 08:02:00+00:00", 1),
    ("p4", "1999000111@lid", "1999000111", "from lid", "2025-01-05 08:00:00+00:00", 0),
]


@pytest.fixture
def contacts_on(store_dir, tmp_path, monkeypatch):
    """store_dir plus a fake AddressBook and Contacts-eligible chats; Contacts enabled."""
    enable_contacts(
        tmp_path / "ab",
        monkeypatch,
        {
            "a": [
                ("Mia", "Gohman", "", ["0585529067"]),
                ("Lid", "Trap", "", ["+1 999000111"]),
                ("Dan", "Levi", "", ["+972 52 000 1111"]),
            ],
            "b": [("Mia", "Gohman", "", ["+972 58-552-9067"])],
        },
        default="a",
    )
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.executemany("INSERT INTO chats VALUES (?, ?, ?)", CONTACTS_CHATS)
    conn.executemany(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
        "VALUES (?, ?, ?, ?, ?, ?)",
        CONTACTS_MESSAGES,
    )
    conn.execute("INSERT INTO messages_fts(messages_fts) VALUES ('rebuild')")
    conn.commit()
    conn.close()
    return store_dir
