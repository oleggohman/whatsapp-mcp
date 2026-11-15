"""Query functions in whatsapp.py against a temp DB: filters, pagination, errors, stdout silence."""

import sqlite3
import time
from datetime import datetime

import pytest
from conftest import make_messages_db, make_whatsapp_db

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
    assert (
        "group hello" in out
        and "from lid" in out
        and "hello\n" not in out.replace("group hello", "")
    )
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
    assert (
        whatsapp.list_messages(chat_jid=ALICE, limit=2, page=5, include_context=False)
        == "No messages to display."
    )


def test_list_messages_context(store_dir):
    out = whatsapp.list_messages(query="second", context_before=1, context_after=1)
    assert out.index("hello") < out.index("second") < out.index("third")
    out = whatsapp.list_messages(query="second", context_before=0, context_after=0)
    assert "hello" not in out and "third" not in out


def test_list_messages_with_context_opens_one_connection(store_dir, monkeypatch):
    opened = []
    real = whatsapp._connect

    def counting():
        opened.append(1)
        return real()

    monkeypatch.setattr(whatsapp, "_connect", counting)
    out = whatsapp.list_messages(query="hello", context_before=1, context_after=1)
    assert "group hello" in out and "Alice" in out
    assert len(opened) == 1

    # Five hits with context still use a single connection.
    opened.clear()
    out = whatsapp.list_messages(limit=5)
    assert len(opened) == 1 and "second" in out and "from lid" in out


def test_list_messages_context_survives_concurrent_chat_merge(store_dir, monkeypatch):
    # The bridge's LID -> PN merge moves rows to another chat in its own transaction. A move
    # committed between the hit query and the context lookup must not fail the listing.
    real = whatsapp._name_resolver

    def merge_then_resolve(conn):
        writer = sqlite3.connect(whatsapp._db_path())
        writer.execute(
            "INSERT INTO chats VALUES ('15550001111@s.whatsapp.net', 'Alice PN', "
            "'2025-01-01 10:02:00+00:00')"
        )
        writer.execute(
            "UPDATE messages SET chat_jid = '15550001111@s.whatsapp.net' "
            "WHERE chat_jid = '111@s.whatsapp.net'"
        )
        writer.commit()
        writer.close()
        return real(conn)

    monkeypatch.setattr(whatsapp, "_name_resolver", merge_then_resolve)
    out = whatsapp.list_messages(query="second", context_before=1, context_after=1)
    assert out.index("hello") < out.index("second") < out.index("third")


def test_list_messages_on_rollback_journal_database(tmp_path, monkeypatch):
    # Documented cost of a non-WAL file, not a goal: the read transaction holds a shared lock
    # until the call ends, so a writer started in the middle of list_messages cannot commit.
    db = tmp_path / "messages.db"
    make_messages_db(db, wal=False)
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(db))
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path))
    real = whatsapp._name_resolver
    outcome = []

    def write_then_resolve(conn):
        writer = sqlite3.connect(db, timeout=0)
        try:
            with pytest.raises(sqlite3.OperationalError, match="database is locked"):
                writer.execute("UPDATE messages SET content = 'changed' WHERE id = 'm1'")
                writer.commit()
            outcome.append("locked")
        finally:
            writer.close()
        return real(conn)

    monkeypatch.setattr(whatsapp, "_name_resolver", write_then_resolve)
    out = whatsapp.list_messages(query="second", context_before=1, context_after=1)
    assert outcome == ["locked"]
    assert out.index("hello") < out.index("second") < out.index("third")


def test_sender_names_do_not_match_by_substring(tmp_path, monkeypatch):
    db = tmp_path / "messages.db"
    make_messages_db(db)
    conn = sqlite3.connect(db)
    conn.executemany(
        "INSERT INTO chats VALUES (?, ?, ?)",
        [
            ("551212@s.whatsapp.net", "Short", "2025-01-01 08:00:00+00:00"),
            ("15551212@s.whatsapp.net", "Long", "2025-01-01 08:00:00+00:00"),
        ],
    )
    conn.commit()
    conn.close()
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(db))
    assert whatsapp.get_sender_name("551212@s.whatsapp.net") == "Short"
    assert whatsapp.get_sender_name("15551212@s.whatsapp.net") == "Long"
    assert whatsapp.get_sender_name("1212") == "1212"  # no LIKE fallback
    assert whatsapp.get_sender_name("5551212") == "5551212"


def test_message_context_picks_requested_chat_for_shared_id(store_dir):
    # The same message id exists in two chats; chat_jid decides which one is used.
    path = whatsapp._db_path()
    conn = sqlite3.connect(path)
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
        "VALUES ('m1', '222@s.whatsapp.net', '222', 'bob m1', '2025-01-01 11:00:00+00:00', 0)"
    )
    conn.commit()
    conn.close()
    with whatsapp._connect() as c:
        ctx = whatsapp._message_context(c, "m1", 1, 1, chat_jid="111@s.whatsapp.net")
        assert ctx.message.content == "hello" and ctx.after[0].content == "second"
        ctx = whatsapp._message_context(c, "m1", 1, 1, chat_jid="222@s.whatsapp.net")
        assert ctx.message.content == "bob m1" and ctx.before[0].content == "bob says hi"
        # Without chat_jid the newest row wins.
        assert whatsapp._message_context(c, "m1", 0, 0).message.content == "bob m1"
    out = whatsapp.list_messages(query="hello", context_before=0, context_after=0)
    assert "group hello" in out and "bob m1" not in out


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
    # The newest of: sender 111 anywhere (group hello, day 2) and the direct chat (third).
    assert "group hello" in whatsapp.get_last_interaction(ALICE)
    assert "group hello" in whatsapp.get_last_interaction("111")
    assert "bob says hi" in whatsapp.get_last_interaction("222@s.whatsapp.net")
    assert "from lid" in whatsapp.get_last_interaction("999000@lid")
    assert whatsapp.get_last_interaction("none") is None


def test_get_last_interaction_group_jid(store_dir):
    # Any other JID matches its own chat exactly, as on main: a group returns its newest message.
    assert "group hello" in whatsapp.get_last_interaction("g1@g.us")
    _add_message(store_dir, "m7", "g1@g.us", "222", "bob in group", "2025-01-02 09:00:00+00:00")
    assert "bob in group" in whatsapp.get_last_interaction("g1@g.us")
    assert whatsapp.get_last_interaction("g2@g.us") is None


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


def test_store_path_with_uri_special_characters(tmp_path, monkeypatch):
    """URI-special characters (#, ?, %, space) in the store path must not break the DB URIs."""
    store = tmp_path / "uri t#1?x%20y"
    store.mkdir()
    make_messages_db(store / "messages.db")
    make_whatsapp_db(store / "whatsapp.db")
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(store / "messages.db"))
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(store))
    assert [c.jid for c in whatsapp.list_chats(query="Alice")] == [ALICE]
    # The LID phone search needs the attached whatsapp.db
    assert [c.jid for c in whatsapp.list_chats(query="15551234567")] == ["999000@lid"]
    with whatsapp._connect() as conn, pytest.raises(sqlite3.OperationalError):
        conn.execute("DELETE FROM chats")  # still read-only


def test_list_messages_bounds_use_driver_layout_in_local_zone(store_dir, monkeypatch):
    monkeypatch.setenv("TZ", "America/New_York")
    time.tzset()
    try:
        conn = sqlite3.connect(store_dir / "messages.db")
        rows = [
            ("z1", "2024-06-01 11:59:59-04:00", "before bound"),
            ("z2", "2024-06-01 12:00:00-04:00", "on bound"),  # zero fraction, non-UTC offset
            ("z3", "2024-06-01 12:00:00.5-04:00", "after bound"),  # trimmed fraction
        ]
        for mid, ts, content in rows:
            conn.execute(
                "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
                "VALUES (?, ?, '111', ?, ?, 0)",
                (mid, ALICE, content, ts),
            )
        conn.commit()
        conn.close()

        assert whatsapp._format_bound(datetime.fromisoformat("2024-06-01T12:00:00")) == (
            "2024-06-01 12:00:00-04:00"
        )
        assert whatsapp._format_bound(datetime.fromisoformat("2024-06-01T12:00:00.250000")) == (
            "2024-06-01 12:00:00.25-04:00"
        )

        out = whatsapp.list_messages(
            chat_jid=ALICE, after="2024-06-01T12:00:00", include_context=False
        )
        assert "after bound" in out and "on bound" not in out and "before bound" not in out

        out = whatsapp.list_messages(
            chat_jid=ALICE,
            after="2024-06-01T00:00:00",
            before="2024-06-01T12:00:00",
            include_context=False,
        )
        assert "before bound" in out and "on bound" not in out and "after bound" not in out

        # An aware bound is converted to the local zone before the text comparison
        assert whatsapp._format_bound(datetime.fromisoformat("2024-06-01T16:00:00+00:00")) == (
            "2024-06-01 12:00:00-04:00"
        )
        out = whatsapp.list_messages(
            chat_jid=ALICE, after="2024-06-01T16:00:00+00:00", include_context=False
        )
        assert "after bound" in out and "on bound" not in out and "before bound" not in out
    finally:
        monkeypatch.undo()
        time.tzset()


# --- last-message shape, contact chats, plans --------------------------------------------------


def _add_message(store_dir, mid, chat, sender, content, ts, from_me=0):
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
        "VALUES (?, ?, ?, ?, ?, ?)",
        (mid, chat, sender, content, ts, from_me),
    )
    conn.commit()
    conn.close()


def test_list_chats_tied_timestamps_one_row_highest_rowid(store_dir):
    # Same timestamp as m3 (the chat's last_message_time): higher rowid wins, no duplicate row.
    _add_message(store_dir, "m3b", ALICE, "111", "tied", "2025-01-01 10:02:00+00:00")
    rows = [c for c in whatsapp.list_chats() if c.jid == ALICE]
    assert len(rows) == 1 and rows[0].last_message == "tied"
    assert whatsapp.get_chat(ALICE).last_message == "tied"
    assert whatsapp.get_direct_chat_by_contact("111").last_message == "tied"


def test_last_message_does_not_need_matching_last_message_time(store_dir):
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute(
        "UPDATE chats SET last_message_time = '2030-01-01 00:00:00+00:00' WHERE jid = ?", (ALICE,)
    )
    conn.commit()
    conn.close()
    assert whatsapp.get_chat(ALICE).last_message == "third"


@pytest.mark.parametrize("arg", ["111", ALICE])
def test_get_contact_chats_bare_sender_and_full_jid(store_dir, arg):
    chats = whatsapp.get_contact_chats(arg)
    assert [c.jid for c in chats] == ["g1@g.us", ALICE]  # last_message_time DESC, one row each
    assert all(c.last_message and c.last_sender for c in chats)
    assert chats[1].last_message == "third" and chats[1].last_is_from_me
    assert "222@s.whatsapp.net" not in {c.jid for c in chats}
    assert len(whatsapp.get_contact_chats(arg, limit=1, page=1)) == 1


def test_get_contact_chats_one_row_per_chat(store_dir):
    _add_message(store_dir, "m7", "g1@g.us", "111", "again", "2025-01-02 08:05:00+00:00")
    jids = [c.jid for c in whatsapp.get_contact_chats("111")]
    assert sorted(jids) == sorted(set(jids))


def test_get_contact_chats_group_jid(store_dir):
    # Any other JID matches its own chat exactly, as on main.
    chats = whatsapp.get_contact_chats("g1@g.us")
    assert [c.jid for c in chats] == ["g1@g.us"]
    assert chats[0].last_message == "group hello"
    assert whatsapp.get_contact_chats("g2@g.us") == []


def test_group_jid_does_not_match_a_contact_with_the_same_user_part(store_dir):
    gid = "120363000000000001"
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute("INSERT INTO chats VALUES (?, 'Other', NULL)", (gid + "@g.us",))
    conn.execute("INSERT INTO chats VALUES (?, 'Lid', NULL)", (gid + "@lid",))
    conn.commit()
    conn.close()
    _add_message(
        store_dir, "g-1", gid + "@g.us", "222", "in the group", "2025-02-01 08:00:00+00:00"
    )
    _add_message(
        store_dir, "g-2", gid + "@lid", gid, "newer lid message", "2025-03-01 08:00:00+00:00"
    )
    _add_message(store_dir, "g-3", "g1@g.us", gid, "sent as the id", "2025-02-02 08:00:00+00:00")

    assert [c.jid for c in whatsapp.get_contact_chats(gid + "@g.us")] == [gid + "@g.us"]
    last = whatsapp.get_last_interaction(gid + "@g.us")
    assert "in the group" in last and "newer lid message" not in last

    assert {c.jid for c in whatsapp.get_contact_chats(gid)} == {gid + "@lid", "g1@g.us"}
    assert "newer lid message" in whatsapp.get_last_interaction(gid)


def test_get_direct_chat_by_contact_is_exact(store_dir):
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute("INSERT INTO chats VALUES ('15551212@s.whatsapp.net', 'Long', NULL)")
    conn.commit()
    conn.close()
    assert whatsapp.get_direct_chat_by_contact("551212") is None
    assert whatsapp.get_direct_chat_by_contact("+1 555-1212").jid == "15551212@s.whatsapp.net"
    assert whatsapp.get_direct_chat_by_contact("15551212@s.whatsapp.net").last_message is None
    assert whatsapp.get_direct_chat_by_contact("999000").jid == "999000@lid"


def test_get_direct_chat_prefers_phone_jid_over_lid(store_dir):
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute("INSERT INTO chats VALUES ('999000@s.whatsapp.net', 'Phone', NULL)")
    conn.commit()
    conn.close()
    assert whatsapp.get_direct_chat_by_contact("999000").jid == "999000@s.whatsapp.net"


def test_list_chats_query_without_last_message_and_lid_map(store_dir):
    chats = whatsapp.list_chats(query="15551234567", include_last_message=False)
    assert [c.jid for c in chats] == ["999000@lid"]  # found through whatsmeow_lid_map
    assert chats[0].last_message is None and chats[0].last_sender is None
    assert chats[0].last_is_from_me is None
    with_last = whatsapp.list_chats(query="15551234567")
    assert with_last[0].last_message == "from lid"
    assert [c.name for c in whatsapp.list_chats(query="a", sort_by="name")] == sorted(
        c.name for c in whatsapp.list_chats(query="a")
    )


def _plan(store_dir, sql, params):
    conn = sqlite3.connect(store_dir / "messages.db")
    try:
        rows = conn.execute("EXPLAIN QUERY PLAN " + sql, params).fetchall()
    finally:
        conn.close()
    return " ".join(r[-1] for r in rows)


def test_last_message_lookup_uses_chat_time_index(store_dir):
    sql = "SELECT 1 FROM messages WHERE rowid = " + whatsapp._LAST_ROWID.format(a="m2", jid="?")
    assert "INDEX idx_messages_chat_time" in _plan(store_dir, sql, (ALICE,))


def test_last_interaction_uses_sender_and_chat_indexes(store_dir):
    plan = _plan(store_dir, whatsapp._LAST_INTERACTION_SQL, ("111", "111", "111", ALICE))
    assert "idx_messages_sender_time" in plan
    assert "idx_messages_chat_time" in plan
    assert "SCAN m " not in plan + " "  # every branch, the exact-JID one too, is an index search


def test_contact_chats_sender_lookup_uses_sender_index(store_dir):
    plan = _plan(store_dir, whatsapp._CONTACT_CHATS_SQL, ("111", "111", ALICE, "111", 20, 0))
    assert "INDEX idx_messages_sender_time" in plan


def test_group_lookups_use_indexes(store_dir):
    last = _plan(store_dir, whatsapp._LAST_CHAT_MESSAGE_SQL, ("g1@g.us",))
    assert "sqlite_autoindex_chats_1" in last
    assert "idx_messages_chat_time" in last
    chat = _plan(store_dir, whatsapp._GROUP_CHAT_SQL, ("g1@g.us", 20, 0))
    assert "sqlite_autoindex_chats_1" in chat
    assert "idx_messages_chat_time" in chat


# --- FTS search (Task 5c) ---


def _add_search_message(store_dir, msg_id, content, chat=ALICE, ts="2025-01-04 10:00:00+00:00"):
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute(
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) "
        "VALUES (?, ?, '111', ?, ?, 0)",
        (msg_id, chat, content, ts),
    )
    conn.commit()
    conn.close()


def _search_ids(query):
    return _ids(whatsapp.list_messages(query=query, include_context=False))


def test_search_substring_hebrew_and_case_folding(store_dir, caplog):
    _add_search_message(store_dir, "h1", "הלכתי בבית")
    with caplog.at_level("WARNING", logger="whatsapp"):
        out = whatsapp.list_messages(query="בית", include_context=False)
        assert "הלכתי בבית" in out
        assert "GROUP hello".lower() in whatsapp.list_messages(query="GROUP", include_context=False)
        assert "hello" in whatsapp.list_messages(query="HELLO", include_context=False)
    assert "falling back to LIKE" not in caplog.text


def test_search_of_three_or_more_chars_is_answered_by_fts(store_dir, caplog):
    # Empty the FTS index but keep messages: only the LIKE path could still find rows.
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute("INSERT INTO messages_fts(messages_fts) VALUES ('delete-all')")
    conn.commit()
    conn.close()
    with caplog.at_level("WARNING", logger="whatsapp"):
        assert _search_ids("bob says") == ["No messages to display."]
        assert _search_ids("hello") == ["No messages to display."]
        # A 2-character query uses LIKE and still finds the row
        assert "bob says hi" in whatsapp.list_messages(query="hi", include_context=False)
    assert "falling back to LIKE" not in caplog.text


def test_search_non_match(store_dir):
    assert whatsapp.list_messages(query="zzzzz", include_context=False) == "No messages to display."


def test_search_quote_in_query_does_not_error(store_dir):
    _add_search_message(store_dir, "q1", 'he said "hi" loudly')
    assert 'he said "hi" loudly' in whatsapp.list_messages(query='"hi"', include_context=False)
    assert whatsapp.list_messages(query='a"b"c', include_context=False) == (
        "No messages to display."
    )


def test_search_short_query_uses_like(store_dir, monkeypatch):
    def boom(_text):
        raise AssertionError("FTS must not be used for short queries")

    monkeypatch.setattr(whatsapp, "_fts_phrase", boom)
    assert "bob says hi" in whatsapp.list_messages(query="bo", include_context=False)
    assert "second" in whatsapp.list_messages(query="d", include_context=False)


def test_search_percent_is_literal_on_fts_path(store_dir, caplog):
    _add_search_message(store_dir, "p1", "got 50% off today")
    with caplog.at_level("WARNING", logger="whatsapp"):
        out = whatsapp.list_messages(query="50% off", include_context=False)
        assert "got 50% off today" in out and "hello" not in out
        # As a LIKE wildcard "hel%" would match "hello"; literally it matches nothing.
        assert whatsapp.list_messages(query="hel%", include_context=False) == (
            "No messages to display."
        )
    assert "falling back to LIKE" not in caplog.text


def _drop_fts(store_dir):
    conn = sqlite3.connect(store_dir / "messages.db")
    for trig in ("insert", "delete", "update"):
        conn.execute(f"DROP TRIGGER messages_fts_{trig}")
    conn.execute("DROP TABLE messages_fts")
    conn.commit()
    conn.close()


def test_like_path_treats_percent_and_underscore_literally(store_dir):
    _add_search_message(store_dir, "l1", "abc")
    _add_search_message(store_dir, "l2", "a_c 5% done")
    _add_search_message(store_dir, "l3", "back\\slash")
    # 2-character queries use LIKE even with the FTS table present.
    assert "a_c 5% done" in whatsapp.list_messages(query="5%", include_context=False)
    assert "abc" not in whatsapp.list_messages(query="5%", include_context=False)
    assert whatsapp.list_messages(query="_c", include_context=True).count("a_c 5% done") == 1
    _drop_fts(store_dir)
    out = whatsapp.list_messages(query="a_c", include_context=False)
    assert "a_c 5% done" in out and "abc" not in out.replace("a_c 5% done", "")
    assert whatsapp.list_messages(query="%%%", include_context=False) == "No messages to display."
    assert "back\\slash" in whatsapp.list_messages(query="k\\s", include_context=False)


def test_search_without_fts_table_falls_back_to_like(store_dir):
    _drop_fts(store_dir)
    assert "bob says hi" in whatsapp.list_messages(query="BOB says", include_context=False)


def test_search_with_missing_fts_trigger_falls_back_to_like(store_dir):
    # A table without its insert trigger no longer sees writes, so it must not answer MATCH.
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute("DROP TRIGGER messages_fts_insert")
    conn.commit()
    conn.close()
    _add_search_message(store_dir, "late", "kingfisher")
    assert "kingfisher" in whatsapp.list_messages(query="kingfisher", include_context=False)


def test_search_match_error_falls_back_to_like(store_dir, caplog):
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.execute("DROP TABLE messages_fts_data")  # break the index but keep messages_fts
    conn.commit()
    conn.close()
    with caplog.at_level("WARNING", logger="whatsapp"):
        out = whatsapp.list_messages(query="bob says", include_context=False)
    assert "bob says hi" in out
    assert "falling back to LIKE" in caplog.text


def test_search_plan_reads_messages_fts(store_dir):
    with whatsapp._connect() as conn:
        plan = " ".join(
            row[3]
            for row in conn.execute(
                "EXPLAIN QUERY PLAN SELECT messages.id FROM messages WHERE " + whatsapp._FTS_CLAUSE,
                (whatsapp._fts_phrase("hello"),),
            )
        )
    assert "messages_fts" in plan


# --- macOS Contacts name fallback --------------------------------------------------------------

MIA = "972585529067@s.whatsapp.net"
LID_CHAT = "1999000111@lid"


def _chat_names(chats) -> dict:
    return {c.jid: c.name for c in chats}


def test_chat_names_fall_back_to_contacts(contacts_on):
    names = _chat_names(whatsapp.list_chats(limit=50))
    assert names[MIA] == "Mia Gohman"
    assert names[ALICE] == "Alice"  # WhatsApp name wins
    assert names["g1@g.us"] == "HY.group Support"
    assert names[LID_CHAT] == "LID 1999000111"  # LID chats are never renamed
    assert names["12345585529067@s.whatsapp.net"] == "Stranger"
    assert whatsapp.get_chat(MIA).name == "Mia Gohman"
    assert whatsapp.get_chat(ALICE).name == "Alice"
    assert whatsapp.get_direct_chat_by_contact("+972 585529067").name == "Mia Gohman"
    chats = whatsapp.get_contact_chats("972585529067")
    assert [c.name for c in chats] == ["Mia Gohman"]


def test_chat_names_stay_stored_when_contacts_off(contacts_on, monkeypatch):
    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", "0")
    assert whatsapp.get_chat(MIA).name == "+972585529067"
    assert _chat_names(whatsapp.list_chats(limit=50))[MIA] == "+972585529067"
    assert whatsapp.get_direct_chat_by_contact("972585529067").name == "+972585529067"
    assert whatsapp.get_contact_chats("972585529067")[0].name == "+972585529067"


def test_message_chat_names_fall_back_to_contacts(contacts_on):
    out = whatsapp.list_messages(chat_jid=MIA, include_context=False)
    assert "Chat: Mia Gohman" in out and "From: Mia Gohman:" in out
    ctx = whatsapp.get_message_context("p2", before=1, after=1)
    assert ctx.message.chat_name == "Mia Gohman"
    assert [m.chat_name for m in ctx.before + ctx.after] == ["Mia Gohman", "Mia Gohman"]
    assert "Chat: Mia Gohman" in whatsapp.get_last_interaction("972585529067")
    lid_out = whatsapp.list_messages(chat_jid=LID_CHAT, include_context=False)
    assert "Chat: LID 1999000111" in lid_out and "Lid Trap" not in lid_out


def test_name_resolver_rules(contacts_on):
    writer = sqlite3.connect(contacts_on / "messages.db")
    writer.execute("INSERT INTO chats VALUES ('972520002222@s.whatsapp.net', NULL, NULL)")
    writer.execute("INSERT INTO chats VALUES ('888888@lid', NULL, NULL)")
    writer.commit()
    writer.close()
    with whatsapp._connect() as conn:
        resolve = whatsapp._name_resolver(conn)
        assert resolve("972585529067") == "Mia Gohman"  # placeholder @s.whatsapp.net row
        assert resolve("972585529067:12") == "Mia Gohman"  # device suffix dropped
        assert resolve("1999000111") == "LID 1999000111"  # only a @lid row: no Contacts
        assert resolve("1999000111@lid") == "LID 1999000111"
        assert resolve("111") == "Alice"
        assert resolve("972520001111") == "Dan Levi"  # no chat row, number in Contacts
        assert resolve("5550000") == "5550000"  # no chat row, not in Contacts
        assert resolve("777777@lid") == "777777@lid"  # LID without a row
        assert resolve("222@lid") == "Bob"  # no lid row: stored WhatsApp name, no Contacts
        assert resolve("972520002222") == "972520002222"  # nameless row, not in Contacts
        assert resolve("888888") == "888888"  # nameless @lid row
        assert resolve("972585529067") == "Mia Gohman"  # memoized


def test_name_resolver_skips_contacts_for_known_lid(contacts_on):
    # Bare LID digits with no chat row (a group sender the bridge could not map when stored)
    # that happen to match a Contacts number.
    wa = sqlite3.connect(contacts_on / "whatsapp.db")
    wa.execute("INSERT INTO whatsmeow_lid_map VALUES ('972520001111', '15550001111')")
    wa.commit()
    wa.close()
    with whatsapp._connect() as conn:
        resolve = whatsapp._name_resolver(conn)
        assert resolve("972520001111") == "972520001111"
        assert resolve("972585529067") == "Mia Gohman"
    (contacts_on / "whatsapp.db").unlink()
    with whatsapp._connect() as conn:  # no LID map: the bare sender is taken as a phone number
        assert whatsapp._name_resolver(conn)("972520001111") == "Dan Levi"


def test_name_resolver_database_error_resolves_to_sender(contacts_on):
    with whatsapp._connect() as conn:
        resolve = whatsapp._name_resolver(conn)
        conn.close()  # any query now raises sqlite3.ProgrammingError
        assert resolve("111") == "111"


def test_search_contacts_by_contacts_name(contacts_on):
    expected = [
        whatsapp.Contact(phone_number="972585529067", name="Mia Gohman", jid=MIA),
    ]
    assert whatsapp.search_contacts("Mia Gohman") == expected
    assert whatsapp.search_contacts("Mia") == expected  # no duplicate for the second entry
    assert [c.name for c in whatsapp.search_contacts("Alice")] == ["Alice"]
    assert whatsapp.search_contacts("Lid Trap") == []  # LID chats are not matched
    assert whatsapp.search_contacts("Dan Levi") == []  # no WhatsApp chat for that entry


def test_search_contacts_ignores_contacts_when_off(contacts_on, monkeypatch):
    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", "0")
    assert whatsapp.search_contacts("Mia Gohman") == []


@pytest.mark.parametrize("sql_hits,expect_mia", [(50, False), (49, True)])
def test_search_contacts_caps_at_50(store_dir, tmp_path, monkeypatch, sql_hits, expect_mia):
    from conftest import enable_contacts

    enable_contacts(tmp_path / "ab", monkeypatch, {"": [("Bulk", "Mia", "", ["+972585529067"])]})
    conn = sqlite3.connect(store_dir / "messages.db")
    conn.executemany(
        "INSERT INTO chats VALUES (?, ?, ?)",
        [(f"9725000000{n:02d}@s.whatsapp.net", f"Bulk {n:02d}", None) for n in range(sql_hits)],
    )
    conn.execute("INSERT INTO chats VALUES (?, ?, ?)", (MIA, "+972585529067", None))
    conn.commit()
    conn.close()
    result = whatsapp.search_contacts("Bulk")
    assert len(result) == 50
    assert (MIA in [c.jid for c in result]) is expect_mia
    if expect_mia:
        assert [c.name for c in result if c.jid == MIA] == ["Bulk Mia"]


@pytest.mark.parametrize(
    "name,jid,expected",
    [
        ("", "972585529067@s.whatsapp.net", True),
        (None, "972585529067@s.whatsapp.net", True),
        ("972585529067", "972585529067@s.whatsapp.net", True),
        ("+972 58-552 9067", "972585529067@s.whatsapp.net", True),
        ("LID 123", "123@lid", True),
        ("Group g1", "g1@g.us", True),
        ("HY.group Support", "g1@g.us", False),
        ("Alice", "111@s.whatsapp.net", False),
        ("+1 Support", "111@s.whatsapp.net", False),
    ],
)
def test_is_placeholder_name(name, jid, expected):
    assert whatsapp._is_placeholder_name(name, jid) is expected


def test_user_part_drops_device_suffix():
    assert whatsapp._user_part("972585529067:12@s.whatsapp.net") == "972585529067"
    assert whatsapp._user_part("972585529067") == "972585529067"
