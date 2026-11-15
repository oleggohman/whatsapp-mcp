"""Read-only name lookup in the macOS Contacts (AddressBook) databases.

Everything stays local: the databases are opened read-only, the result lives in memory
only, nothing is written to the WhatsApp store and nothing leaves the machine. The module
is inert off macOS and when WHATSAPP_MACOS_CONTACTS is "0", "false", "no" or "off". A
missing directory or denied Contacts access yields an empty index, never an exception.
"""

import logging
import os
import plistlib
import re
import sqlite3
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from xml.parsers.expat import ExpatError

logger = logging.getLogger(__name__)

ENV_VAR = "WHATSAPP_MACOS_CONTACTS"
MIN_NATIONAL_DIGITS = 7
MAX_NATIONAL_DIGITS = 11
REFRESH_SECONDS = 60.0
DB_NAME = "AddressBook-v22.abcddb"

_QUERY = (
    "SELECT r.ZFIRSTNAME, r.ZLASTNAME, r.ZORGANIZATION, p.ZFULLNUMBER "
    "FROM ZABCDPHONENUMBER p JOIN ZABCDRECORD r ON r.Z_PK = p.ZOWNER "
    "WHERE p.ZFULLNUMBER IS NOT NULL"
)


@dataclass(frozen=True)
class Entry:
    name: str
    full: str
    national: str


@dataclass(frozen=True)
class Index:
    full: dict[str, str]
    national: dict[str, str]
    entries: list[Entry]

    def name_for(self, digits: str) -> str | None:
        """Name for a WhatsApp number given as international digits, or None."""
        if digits in self.full:
            return self.full[digits]
        # Drop a 1-3 digit country code and match the national number; no country-code
        # table is needed, at the price of accepting any 1-3 digit prefix.
        for k in (1, 2, 3):
            rest = digits[k:]
            if MIN_NATIONAL_DIGITS <= len(rest) <= MAX_NATIONAL_DIGITS and rest in self.national:
                return self.national[rest]
        return None

    def entries_matching(self, query: str) -> list[Entry]:
        needle = query.casefold()
        return [e for e in self.entries if needle in e.name.casefold()]


EMPTY = Index({}, {}, [])

_index: Index | None = None
_checked_at = 0.0
_signature_seen: tuple | None = None
_last_failure: str | None = None


def _is_macos() -> bool:
    return sys.platform == "darwin"


def enabled() -> bool:
    if not _is_macos():
        return False
    return os.environ.get(ENV_VAR, "1").strip().lower() not in ("0", "false", "no", "off")


def addressbook_dir() -> Path:
    return Path.home() / "Library" / "Application Support" / "AddressBook"


def preferences_path() -> Path:
    return Path.home() / "Library" / "Preferences" / "com.apple.AddressBook.plist"


def normalize(raw: str) -> tuple[str, str]:
    """Return (full, national) digits of a Contacts number.

    A leading "+" or "00" marks an international number: full is the digits after it and
    national equals full. Otherwise full is the digits as written and national drops the
    leading zeros.
    """
    digits = re.sub(r"\D", "", raw)
    if raw.lstrip().startswith("+"):
        return digits, digits
    if digits.startswith("00"):
        return digits[2:], digits[2:]
    return digits, digits.lstrip("0")


def _default_source() -> str | None:
    try:
        with preferences_path().open("rb") as f:
            data = plistlib.load(f)
    except (OSError, plistlib.InvalidFileException, ExpatError, ValueError):
        return None
    value = data.get("ABDefaultSourceID") if isinstance(data, dict) else None
    return value if isinstance(value, str) else None


def _source_paths() -> list[Path]:
    """Databases to read, most preferred first: default source, other sources, root."""
    root = addressbook_dir()
    if not root.is_dir():
        return []
    default = _default_source()
    sources = root / "Sources"
    dirs = sorted(sources.iterdir()) if sources.is_dir() else []
    dirs.sort(key=lambda d: d.name != default)  # stable: the default source goes first
    paths = [d / DB_NAME for d in dirs] + [root / DB_NAME]
    return [p for p in paths if p.is_file()]


def _signature(paths: list[Path]) -> tuple:
    sig = []
    for path in paths:
        for f in (path, path.with_name(path.name + "-wal")):
            try:
                st = f.stat()
            except FileNotFoundError:
                sig.append((str(f), None, None))
            else:
                sig.append((str(f), st.st_mtime_ns, st.st_size))
    return tuple(sig)


def _read_rows(uri: str) -> list[tuple]:
    conn = sqlite3.connect(uri, uri=True)
    try:
        return conn.execute(_QUERY).fetchall()
    finally:
        conn.close()


def _rows(path: Path) -> list[tuple]:
    uri = path.as_uri()
    try:
        return _read_rows(uri + "?mode=ro")
    except sqlite3.OperationalError:
        # mode=ro on a WAL database needs the -shm; when that fails, immutable=1 reads the
        # main file only and may miss edits since the last checkpoint. Better than nothing,
        # never a write.
        return _read_rows(uri + "?immutable=1")


def _text(value: object) -> str:
    """A stripped text column, or "" for NULL and non-text values."""
    return value.strip() if isinstance(value, str) else ""


def _load(paths: list[Path]) -> Index:
    full: dict[str, str] = {}
    national: dict[str, str] = {}
    entries: list[Entry] = []
    for path in paths:
        try:
            rows = _rows(path)
        except sqlite3.DatabaseError:
            continue
        for first, last, org, number in rows:
            if not isinstance(number, str):
                continue  # a BLOB or number in ZFULLNUMBER: skip the row, keep the index
            name = " ".join(p for p in (_text(first), _text(last)) if p)
            name = name or _text(org)
            n_full, n_national = normalize(number)
            if not name or len(n_national) < MIN_NATIONAL_DIGITS:
                continue
            full.setdefault(n_full, name)
            national.setdefault(n_national, name)
            entries.append(Entry(name, n_full, n_national))
    return Index(full, national, entries)


def get_index() -> Index:
    global _index, _checked_at, _signature_seen, _last_failure
    if not enabled():
        return EMPTY
    now = time.monotonic()
    if _index is not None and now - _checked_at < REFRESH_SECONDS:
        return _index
    _checked_at = now
    try:
        paths = _source_paths()
        sig = _signature(paths)
        if _index is not None and sig == _signature_seen:
            return _index
        _index = _load(paths)
        # Taken after the load: opening a WAL database can create its -wal/-shm files,
        # which would otherwise look like a change at the next refresh.
        sig = _signature(paths)
    except (OSError, sqlite3.DatabaseError) as e:
        msg = f"{type(e).__name__}: {e}"
        if msg != _last_failure:
            logger.info(
                "macOS Contacts not readable (grant Contacts access to the app running "
                "the MCP server, or set WHATSAPP_MACOS_CONTACTS=0): %s",
                msg,
            )
            _last_failure = msg
        _index = EMPTY
        _signature_seen = None
        return _index
    _signature_seen = sig
    _last_failure = None
    return _index


def reset() -> None:
    global _index, _checked_at, _signature_seen, _last_failure
    _index = None
    _checked_at = 0.0
    _signature_seen = None
    _last_failure = None
