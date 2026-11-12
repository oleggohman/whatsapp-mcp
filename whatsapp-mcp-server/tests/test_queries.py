"""Query functions in whatsapp.py against a temp DB: filters, pagination, errors, stdout silence."""

import sqlite3

import pytest

import whatsapp

ALICE = "111@s.whatsapp.net"


def _ids(text: str) -> list:
    return [line for line in text.splitlines() if line]


# --- list_messages -----------------------------------------------------------------------------


def test_list_messages_chat_filter_orders_newest_first(store_dir):
    out = whatsapp.list_messages(chat_jid=ALICE, include_context=False)
    lines = _ids(out)
    assert len(lines) == 3
    assert "third" in lines[0] and "hello" in lines[2]


def test_list_messages_sender_and_query_filters(store_dir):
    out = whatsapp.list_messages(sender_phone_number="222", include_context=False)
    assert "bob says hi" in out and "hello" not in out
    out = whatsapp.list_messages(query="GROUP", include_context=False)
    assert "group hello" in out and "bob says hi" not in out


def test_list_messages_date_filters(store_dir):
    out = whatsapp.list_messages(after="2025-01-02T00:00:00", include_context=False)
    assert "group hello" in out and "from lid" in out and "hello\n" not in out.replace("group hello", "")
    out = whatsapp.list_messages(before="2025-01-01T09:30:00", include_context=False)
    assert "bob says hi" in out and "group hello" not in out


@pytest.mark.parametrize("field", ["after", "before"])
def test_list_messages_bad_date(store_dir, field):
    with pytest.raises(ValueError, match=f"'{field}'"):
        whatsapp.list_messages(**{field: "not-a-date"})


def test_list_messages_pagination(store_dir):
    page0 = whatsapp.list_messages(chat_jid=ALICE, limit=2, page=0, include_context=False)
    page1 = whatsapp.list_messages(chat_jid=ALICE, limit=2, page=1, include_context=False)
    assert "third" in page0 and "second" in page0 and "hello" not in page0
    assert "hello" in page1 and "third" not in page1
    assert whatsapp.list_messages(chat_jid=ALICE, limit=2, page=5, include_context=False) == "No messages to display."


def test_list_messages_context(store_dir):
    out = whatsapp.list_messages(query="second", context_before=1, context_after=1)
    assert out.index("hello") < out.index("second") < out.index("third")
    out = whatsapp.list_messages(query="second", context_before=0, context_after=0)
    assert "hello" not in out and "third" not in out


def test_message_context_missing(store_dir):
    with pytest.raises(ValueError, match="not found"):
        whatsapp.get_message_context("nope")


# --- chats / contacts --------------------------------------------------------------------------


def test_list_chats_query_and_sort(store_dir):
    assert [c.name for c in whatsapp.list_chats(query="bob")] == ["Bob"]
    by_active = [c.jid for c in whatsapp.list_chats()]
    assert by_active == ["999000@lid", "g1@g.us", ALICE, "222@s.whatsapp.net"]
    by_name = [c.name for c in whatsapp.list_chats(sort_by="name")]
    assert by_name == sorted(by_name)
    assert len(whatsapp.list_chats(limit=2, page=1)) == 2


def test_list_chats_without_last_message(store_dir):
    chat = next(c for c in whatsapp.list_chats(include_last_message=False) if c.jid == ALICE)
    assert chat.last_message is None


def test_search_contacts_excludes_groups(store_dir):
    assert [c.jid for c in whatsapp.search_contacts("hy.group")] == []
    assert [c.phone_number for c in whatsapp.search_contacts("alice")] == ["111"]


def test_get_chat(store_dir):
    chat = whatsapp.get_chat(ALICE)
    assert chat.name == "Alice" and chat.last_message == "third" and chat.last_is_from_me
    assert whatsapp.get_chat(ALICE, include_last_message=False).last_message is None
    assert whatsapp.get_chat("none@s.whatsapp.net") is None


def test_get_direct_chat_by_contact(store_dir):
    assert whatsapp.get_direct_chat_by_contact("111").jid == ALICE
    assert whatsapp.get_direct_chat_by_contact("g1") is None  # groups are not direct chats


def test_get_last_interaction(store_dir):
    assert "third" in whatsapp.get_last_interaction(ALICE)
    assert whatsapp.get_last_interaction("none") is None


def test_get_sender_name(store_dir):
    assert whatsapp.get_sender_name(ALICE) == "Alice"
    assert whatsapp.get_sender_name("111") == "Alice"
    assert whatsapp.get_sender_name("777") == "777"


# --- connection handling / errors --------------------------------------------------------------


def test_connect_closes_connection(store_dir):
    with whatsapp._connect() as conn:
        conn.execute("SELECT 1")
    with pytest.raises(sqlite3.ProgrammingError):
        conn.execute("SELECT 1")


def test_connect_closes_on_error(store_dir):
    with pytest.raises(KeyError):
        with whatsapp._connect() as conn:
            raise KeyError("boom")
    with pytest.raises(sqlite3.ProgrammingError):
        conn.execute("SELECT 1")


def test_missing_db_is_clean_error(tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(tmp_path / "absent.db"))
    for call in (
        lambda: whatsapp.list_messages(),
        lambda: whatsapp.list_chats(),
        lambda: whatsapp.search_contacts("x"),
        lambda: whatsapp.get_chat("x"),
    ):
        with pytest.raises(RuntimeError, match="database not found"):
            call()
    assert not (tmp_path / "absent.db").exists()  # read-only open must not create it


def test_sqlite_error_is_logged_to_stderr_not_stdout(store_dir, capsys):
    (store_dir / "messages.db").write_bytes(b"this is not a sqlite database" * 100)
    assert whatsapp.list_chats() == []
    assert whatsapp.get_chat("x") is None
    captured = capsys.readouterr()
    assert captured.out == ""


def test_no_stdout_during_queries(store_dir, capsys):
    whatsapp.list_messages(query="second")
    whatsapp.list_chats(query="Alice")
    whatsapp.search_contacts("Bob")
    whatsapp.get_last_interaction(ALICE)
    whatsapp.get_chat(ALICE)
    whatsapp.get_direct_chat_by_contact("222")
    assert capsys.readouterr().out == ""
