"""whatsapp.py error handling and input validation: injected sqlite failures and odd inputs."""

import logging
import sqlite3
from contextlib import contextmanager
from datetime import datetime

import httpx
import pytest

import whatsapp


@pytest.fixture
def db_broken(monkeypatch):
    """Every _connect() fails with a sqlite error, as with a corrupt or locked database."""

    @contextmanager
    def broken():
        raise sqlite3.OperationalError("database is locked")
        yield  # pragma: no cover - makes this a generator for @contextmanager

    monkeypatch.setattr(whatsapp, "_connect", broken)


def _logged(caplog):
    return [r.getMessage() for r in caplog.records if r.levelno >= logging.ERROR]


def test_chat_is_group_follows_jid_suffix():
    assert whatsapp.Chat(jid="g1@g.us", name="G", last_message_time=None).is_group is True
    assert whatsapp.Chat(jid="1@s.whatsapp.net", name="A", last_message_time=None).is_group is False


# --- sqlite failures degrade per function contract -------------------------------------------


def test_sender_name_falls_back_to_jid_on_db_error(db_broken, caplog):
    assert whatsapp.get_sender_name("111@s.whatsapp.net") == "111@s.whatsapp.net"
    assert "Database error while getting sender name: database is locked" in _logged(caplog)


def test_list_messages_returns_empty_listing_on_db_error(db_broken, caplog):
    assert whatsapp.list_messages(query="x") == "No messages to display."
    assert "Database error: database is locked" in _logged(caplog)


def test_message_context_reraises_db_error(db_broken, caplog):
    with pytest.raises(sqlite3.OperationalError):
        whatsapp.get_message_context("m1")
    assert "Database error: database is locked" in _logged(caplog)


def test_search_contacts_and_contact_chats_return_empty_on_db_error(db_broken, caplog):
    assert whatsapp.search_contacts("alice") == []
    assert whatsapp.get_contact_chats("111@s.whatsapp.net") == []
    assert len(_logged(caplog)) == 2


def test_last_interaction_and_get_chat_return_none_on_db_error(db_broken, caplog):
    assert whatsapp.get_last_interaction("111@s.whatsapp.net") is None
    assert whatsapp.get_chat("111@s.whatsapp.net") is None
    assert whatsapp.get_direct_chat_by_contact("111") is None
    assert len(_logged(caplog)) == 3


# --- formatting and lookups --------------------------------------------------------------------


def test_sender_name_for_bare_number_matches_by_substring(store_dir):
    # No "@" in the sender: the number itself is matched against chat JIDs.
    assert whatsapp.get_sender_name("222") == "Bob"
    assert whatsapp.get_sender_name("no-such-number") == "no-such-number"
    # A JID with another server suffix is matched by its number part.
    assert whatsapp.get_sender_name("222@lid") == "Bob"


def _msg(**kw):
    base = dict(
        timestamp=datetime(2025, 1, 1, 10, 0, 0),
        sender="111",
        content="hi",
        is_from_me=True,
        chat_jid="c",
        id="m",
    )
    return whatsapp.Message(**{**base, **kw})


def test_format_message_without_chat_info():
    assert whatsapp.format_message(_msg(chat_name="Alice"), show_chat_info=False) == (
        "[2025-01-01 10:00:00] From: Me: hi\n"
    )
    assert whatsapp.format_message(_msg(chat_name=None)) == "[2025-01-01 10:00:00] From: Me: hi\n"


def test_format_message_survives_sender_lookup_failure(monkeypatch, caplog):
    def boom(sender):
        raise ValueError("lookup exploded")

    monkeypatch.setattr(whatsapp, "get_sender_name", boom)
    out = whatsapp.format_message(_msg(is_from_me=False))
    assert out == "[2025-01-01 10:00:00] "  # header only: the broken body is skipped
    assert "Error formatting message: lookup exploded" in _logged(caplog)


def test_list_messages_without_filters_returns_everything_newest_first(store_dir):
    out = whatsapp.list_messages(include_context=False)
    assert out.index("from lid") < out.index("group hello") < out.index("hello\n")


# --- bridge POST edge cases --------------------------------------------------------------------


def test_post_non_object_json_is_a_parse_error(monkeypatch):
    transport = httpx.MockTransport(lambda request: httpx.Response(200, json=["not", "object"]))
    monkeypatch.setattr(whatsapp, "_http_transport", transport)
    ok, msg = whatsapp.send_message("123", "hi")
    assert ok is False and msg == 'Error parsing response: ["not","object"]'


def test_post_unexpected_exception_is_reported(monkeypatch):
    def handler(request):
        raise RuntimeError("transport bug")

    monkeypatch.setattr(whatsapp, "_http_transport", httpx.MockTransport(handler))
    assert whatsapp.send_message("123", "hi") == (False, "Unexpected error: transport bug")


@pytest.mark.parametrize("send", [whatsapp.send_file, whatsapp.send_audio_message])
def test_media_senders_validate_arguments(send, tmp_path):
    assert send("", "x") == (False, "Recipient must be provided")
    assert send("123", "") == (False, "Media path must be provided")
    missing = str(tmp_path / "missing.ogg")
    assert send("123", missing) == (False, f"Media file not found: {missing}")


def test_audio_conversion_failure_is_reported(tmp_path, monkeypatch):
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")

    def fail(path):
        raise RuntimeError("codec missing")

    monkeypatch.setattr(whatsapp.audio, "convert_to_opus_ogg_temp", fail)
    assert whatsapp.send_audio_message("123", str(src)) == (
        False,
        "Error converting file to opus ogg: codec missing",
    )


def test_temp_file_cleanup_failure_is_logged_not_raised(tmp_path, monkeypatch, caplog):
    src = tmp_path / "voice.mp3"
    src.write_bytes(b"mp3")
    temp = tmp_path / "converted.ogg"
    temp.write_bytes(b"OggS")
    monkeypatch.setattr(whatsapp.audio, "convert_to_opus_ogg_temp", lambda p: str(temp))
    monkeypatch.setattr(whatsapp, "_send", lambda payload: (True, "sent"))

    def deny(path):
        raise PermissionError("denied")

    monkeypatch.setattr(whatsapp.os, "unlink", deny)
    with caplog.at_level(logging.WARNING, logger="whatsapp"):
        assert whatsapp.send_audio_message("123", str(src)) == (True, "sent")
    assert f"could not remove temp file {temp}" in caplog.text
