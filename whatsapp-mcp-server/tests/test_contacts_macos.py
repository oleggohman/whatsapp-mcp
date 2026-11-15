"""contacts_macos against fake AddressBook trees in temp dirs; the real one is never opened."""

import logging
import os
import plistlib
import sqlite3
from pathlib import Path
from types import SimpleNamespace

import pytest
from conftest import make_addressbook

import contacts_macos
from contacts_macos import Index, normalize

MIA = ("Mia", "Gohman", "", ["0585529067"])
DAN = ("Dan", "Levi", "", ["+1 (415) 555-0100"])


@pytest.fixture
def ab(tmp_path, monkeypatch):
    """Build a fake AddressBook tree and point the module at it as if on macOS."""
    root = tmp_path / "AddressBook"
    root.mkdir()
    prefs = root / "prefs.plist"
    monkeypatch.setattr(contacts_macos, "_is_macos", lambda: True)
    monkeypatch.setattr(contacts_macos, "addressbook_dir", lambda: root)
    monkeypatch.setattr(contacts_macos, "preferences_path", lambda: prefs)
    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", "1")
    contacts_macos.reset()

    def build(sources, default=None):
        make_addressbook(root, sources, default)
        return root

    return build


class Clock:
    def __init__(self):
        self.now = 0.0

    def monotonic(self):
        return self.now


@pytest.fixture
def clock(monkeypatch):
    c = Clock()
    monkeypatch.setattr(contacts_macos, "time", SimpleNamespace(monotonic=c.monotonic))
    return c


def test_normalize():
    assert normalize("+972 58-552-9067") == ("972585529067", "972585529067")
    assert normalize("00972585529067") == ("972585529067", "972585529067")
    assert normalize("058-5529067") == ("0585529067", "585529067")
    assert normalize("(03) 123-4567") == ("031234567", "31234567")
    assert normalize("*1234") == ("1234", "1234")


def test_short_numbers_are_skipped(ab):
    ab({"": [("Svc", "", "", ["*1234"]), MIA]})
    index = contacts_macos.get_index()
    assert [e.name for e in index.entries] == ["Mia Gohman"]


def test_name_for(ab):
    ab({"": [MIA, DAN]})
    index = contacts_macos.get_index()
    assert index.name_for("972585529067") == "Mia Gohman"
    assert index.name_for("14155550100") == "Dan Levi"
    # Accepted tolerance: any 1-3 digit prefix is taken for a country code.
    assert index.name_for("1585529067") == "Mia Gohman"
    assert index.name_for("5529067") is None
    assert index.name_for("972500000000") is None


def test_name_for_full_hit_beats_national_hit(ab):
    ab(
        {
            "a": [("Full", "Hit", "", ["+972585529067"])],
            "b": [("National", "Hit", "", ["0585529067"])],
        }
    )
    assert contacts_macos.get_index().name_for("972585529067") == "Full Hit"


def test_name_for_suffix_bounds(ab):
    ab({"": [("Long", "One", "", ["0123456789012"]), ("Eleven", "One", "", ["012345678901"])]})
    index = contacts_macos.get_index()
    # national 123456789012 has 12 digits: never matched by suffix
    assert index.name_for("9123456789012") is None
    assert index.name_for("912345678901") == "Eleven One"
    seven = Index({}, {"1234567": "Seven"}, [])
    six = Index({}, {"123456": "Six"}, [])
    assert seven.name_for("991234567") == "Seven"
    assert six.name_for("9123456") is None


def test_source_order(ab):
    number = ["+972585529067"]
    sources = {
        "a": [("Alpha", "One", "", number)],
        "b": [("Beta", "Two", "", number)],
        "": [("Root", "Guy", "", number), ("Only", "Root", "", ["+972500000001"])],
    }
    ab(sources)
    assert contacts_macos.get_index().name_for("972585529067") == "Alpha One"
    assert contacts_macos.get_index().name_for("972500000001") == "Only Root"
    contacts_macos.reset()
    # default source set: Beta wins
    prefs = contacts_macos.preferences_path()
    prefs.write_bytes(plistlib.dumps({"ABDefaultSourceID": "b"}, fmt=plistlib.FMT_BINARY))
    assert contacts_macos.get_index().name_for("972585529067") == "Beta Two"


def test_names_organization_and_nameless(ab):
    ab(
        {
            "": [
                ("", "", "Acme Ltd", ["+972500000002"]),
                ("", "", "", ["+972500000003"]),
                ("", "Solo", "", ["+972500000004"]),
            ]
        }
    )
    index = contacts_macos.get_index()
    assert index.name_for("972500000002") == "Acme Ltd"
    assert index.name_for("972500000003") is None
    assert index.name_for("972500000004") == "Solo"


def test_non_text_columns_are_skipped(ab):
    root = ab({"": [MIA]})
    conn = sqlite3.connect(root / contacts_macos.DB_NAME)
    conn.execute("INSERT INTO ZABCDRECORD VALUES (7, ?, X'01', X'00')", (b"Blob",))
    conn.execute("INSERT INTO ZABCDRECORD VALUES (8, 'Num', 'Ber', '')")
    conn.execute("INSERT INTO ZABCDRECORD VALUES (9, 'Org', NULL, 3.5)")
    conn.execute("INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (7, '+972500000007')")
    conn.execute("INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (8, X'2B3937')")
    conn.execute("INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (9, '+972500000009')")
    conn.commit()
    conn.close()
    index = contacts_macos.get_index()
    assert index.name_for("972585529067") == "Mia Gohman"
    assert index.name_for("972500000007") is None
    assert index.name_for("972500000009") == "Org"
    assert [e.name for e in index.entries] == ["Mia Gohman", "Org"]


def test_corrupt_plist(ab):
    root = ab(
        {
            "b": [("Beta", "Two", "", ["+972585529067"])],
            "a": [("Alpha", "One", "", ["+972585529067"])],
        }
    )
    (root / "prefs.plist").write_bytes(b"not a plist")
    assert contacts_macos._default_source() is None
    assert contacts_macos.get_index().name_for("972585529067") == "Alpha One"


def test_default_source_variants(ab):
    root = ab({})
    prefs = root / "prefs.plist"
    assert contacts_macos._default_source() is None  # missing file
    prefs.write_bytes(b"<?xml version='1.0'?><plist><dict>")
    assert contacts_macos._default_source() is None  # malformed XML
    prefs.write_bytes(plistlib.dumps([1, 2]))
    assert contacts_macos._default_source() is None  # not a dict
    prefs.write_bytes(plistlib.dumps({"ABDefaultSourceID": 5}))
    assert contacts_macos._default_source() is None  # not a string
    prefs.write_bytes(plistlib.dumps({"ABDefaultSourceID": "uuid-1"}))
    assert contacts_macos._default_source() == "uuid-1"


def test_entries_matching(ab):
    ab({"": [MIA, DAN]})
    index = contacts_macos.get_index()
    assert [e.name for e in index.entries_matching("mia goh")] == ["Mia Gohman"]
    assert index.entries_matching("zzz") == []


@pytest.mark.parametrize("value", ["off", "0", "false", "no", " OFF "])
def test_disabled_by_env(ab, monkeypatch, value):
    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", value)
    assert contacts_macos.enabled() is False


def test_enabled_default_and_off_macos(ab, monkeypatch):
    assert contacts_macos.enabled() is True
    monkeypatch.delenv("WHATSAPP_MACOS_CONTACTS")
    assert contacts_macos.enabled() is True
    monkeypatch.setattr(contacts_macos, "_is_macos", lambda: False)
    assert contacts_macos.enabled() is False


def test_real_platform_check(monkeypatch):
    monkeypatch.setattr(contacts_macos.sys, "platform", "linux")
    assert contacts_macos._is_macos() is False
    monkeypatch.setattr(contacts_macos.sys, "platform", "darwin")
    assert contacts_macos._is_macos() is True


def test_default_paths_are_under_home(monkeypatch, tmp_path):
    monkeypatch.setattr(Path, "home", lambda: tmp_path)
    assert contacts_macos.addressbook_dir() == tmp_path / "Library/Application Support/AddressBook"
    assert contacts_macos.preferences_path() == (
        tmp_path / "Library/Preferences/com.apple.AddressBook.plist"
    )


def test_disabled_never_touches_directory(ab, monkeypatch):
    def boom():
        raise AssertionError("directory must not be touched")

    monkeypatch.setenv("WHATSAPP_MACOS_CONTACTS", "0")
    monkeypatch.setattr(contacts_macos, "addressbook_dir", boom)
    assert contacts_macos.get_index() is contacts_macos.EMPTY


def test_missing_directory_is_silent(ab, monkeypatch, tmp_path, caplog):
    monkeypatch.setattr(contacts_macos, "addressbook_dir", lambda: tmp_path / "nope")
    caplog.set_level(logging.INFO, logger="contacts_macos")
    index = contacts_macos.get_index()
    assert index.entries == []
    assert caplog.records == []


def test_root_without_sources_dir(ab):
    ab({"": [MIA]})
    assert contacts_macos.get_index().name_for("972585529067") == "Mia Gohman"


def _deny_iterdir(monkeypatch, message):
    def iterdir(self):
        raise PermissionError(1, message)

    monkeypatch.setattr(Path, "iterdir", iterdir)


def test_permission_error_logs_once_and_returns_empty(ab, monkeypatch, caplog):
    ab({"a": [MIA]})
    caplog.set_level(logging.INFO, logger="contacts_macos")
    _deny_iterdir(monkeypatch, "Operation not permitted")
    assert contacts_macos.get_index().entries == []
    lines = [r for r in caplog.records if r.name == "contacts_macos"]
    assert len(lines) == 1
    assert lines[0].levelno == logging.INFO
    assert "WHATSAPP_MACOS_CONTACTS=0" in lines[0].getMessage()


def test_failure_logged_once_per_distinct_message(ab, monkeypatch, caplog, clock):
    ab({"a": [MIA]})
    caplog.set_level(logging.INFO, logger="contacts_macos")

    def lines():
        return [r for r in caplog.records if r.name == "contacts_macos"]

    _deny_iterdir(monkeypatch, "denied one")
    for t in (0.0, 61.0, 122.0):
        clock.now = t
        assert contacts_macos.get_index().entries == []
    assert len(lines()) == 1

    clock.now = 183.0
    _deny_iterdir(monkeypatch, "denied two")
    contacts_macos.get_index()
    assert len(lines()) == 2


def test_failure_logged_again_after_success(ab, monkeypatch, caplog, clock):
    ab({"a": [MIA]})
    caplog.set_level(logging.INFO, logger="contacts_macos")
    real_iterdir = Path.iterdir

    def lines():
        return [r for r in caplog.records if r.name == "contacts_macos"]

    _deny_iterdir(monkeypatch, "denied")
    contacts_macos.get_index()
    assert len(lines()) == 1
    monkeypatch.setattr(Path, "iterdir", real_iterdir)
    clock.now = 61.0
    assert [e.name for e in contacts_macos.get_index().entries] == ["Mia Gohman"]
    _deny_iterdir(monkeypatch, "denied")
    clock.now = 122.0
    # unchanged signature would hide the failure only if iterdir were not reached; it is
    assert contacts_macos.get_index().entries == []
    assert len(lines()) == 2


def test_not_a_database_is_skipped(ab, tmp_path):
    root = ab({"b": [MIA]})
    bad = root / "Sources" / "a"
    bad.mkdir(parents=True)
    (bad / contacts_macos.DB_NAME).write_bytes(b"x" * 4096)
    assert contacts_macos.get_index().name_for("972585529067") == "Mia Gohman"


def test_mode_ro_failure_falls_back_to_immutable(ab, monkeypatch):
    ab({"": [MIA]})
    real_connect = sqlite3.connect
    uris = []

    def connect(target, **kwargs):
        uris.append(target)
        if "mode=ro" in target:
            raise sqlite3.OperationalError("unable to open database file")
        return real_connect(target, **kwargs)

    monkeypatch.setattr(sqlite3, "connect", connect)
    assert contacts_macos.get_index().name_for("972585529067") == "Mia Gohman"
    assert [u.rsplit("?", 1)[1] for u in uris] == ["mode=ro", "immutable=1"]


def test_both_open_attempts_failing_skips_file(ab, monkeypatch):
    ab({"": [MIA]})

    def connect(target, **kwargs):
        raise sqlite3.OperationalError("unable to open database file")

    monkeypatch.setattr(sqlite3, "connect", connect)
    assert contacts_macos.get_index().entries == []


def test_cache_refresh_and_rebuild(ab, monkeypatch, clock):
    root = ab({"a": [MIA]})
    real_connect = sqlite3.connect
    real_paths = contacts_macos._source_paths
    connects = []
    stats = []

    def connect(target, **kwargs):
        connects.append(target)
        return real_connect(target, **kwargs)

    def paths():
        stats.append(1)
        return real_paths()

    monkeypatch.setattr(sqlite3, "connect", connect)
    monkeypatch.setattr(contacts_macos, "_source_paths", paths)

    first = contacts_macos.get_index()
    clock.now = 1.0
    assert contacts_macos.get_index() is first
    assert (len(stats), len(connects)) == (1, 1)

    # refresh interval passed, files unchanged: stat again, no rebuild
    clock.now = 61.0
    assert contacts_macos.get_index() is first
    assert (len(stats), len(connects)) == (2, 1)

    # a file changed: rebuilt, new name visible
    db = root / "Sources" / "a" / contacts_macos.DB_NAME
    conn = real_connect(db)
    conn.execute("INSERT INTO ZABCDRECORD VALUES (9, 'New', 'Person', '')")
    conn.execute("INSERT INTO ZABCDPHONENUMBER (ZOWNER, ZFULLNUMBER) VALUES (9, '+972500000009')")
    conn.commit()
    conn.close()
    stamp = os.stat(db).st_mtime + 10
    os.utime(db, (stamp, stamp))
    clock.now = 122.0
    rebuilt = contacts_macos.get_index()
    assert rebuilt is not first
    assert rebuilt.name_for("972500000009") == "New Person"
    assert len(connects) == 2

    # reset() forces a rebuild even inside the refresh window
    contacts_macos.reset()
    contacts_macos.get_index()
    assert len(connects) == 3
