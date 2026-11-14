import logging
import os.path
import pathlib
import sqlite3
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from dataclasses import asdict, dataclass
from datetime import datetime
from typing import Any

import httpx

import audio
import bridge_manager

# stdout is the MCP stdio transport: all diagnostics go through logging (stderr).
logger = logging.getLogger(__name__)

HTTP_TIMEOUT = httpx.Timeout(60.0, connect=5.0)

# Test hook: when set, used as the httpx transport (e.g. httpx.MockTransport).
_http_transport: httpx.BaseTransport | None = None


def _db_path() -> str:
    """Path of messages.db: WHATSAPP_DB_PATH, else <WHATSAPP_STORE_DIR>/messages.db."""
    return os.environ.get("WHATSAPP_DB_PATH") or os.path.join(
        bridge_manager.store_dir(), "messages.db"
    )


def _api_base_url() -> str:
    return bridge_manager.api_base_url()


def _bridge_token() -> str | None:
    """Bearer token: WHATSAPP_BRIDGE_TOKEN, else <store>/bridge.token when it exists."""
    return bridge_manager.read_token()


def _ro_uri(path: str) -> str:
    """Read-only SQLite URI for an absolute path, percent-encoding "#", "?", "%" and the like."""
    return pathlib.Path(path).as_uri() + "?mode=ro"


@contextmanager
def _connect() -> Iterator[sqlite3.Connection]:
    """Open messages.db read-only and attach whatsapp.db (read-only, alias "wa") when it exists.

    The connection is always closed on exit. whatsapp.db sits next to messages.db; it is only
    used for the LID -> phone number map, so a missing file is not an error. A missing
    messages.db raises RuntimeError with an actionable message.
    """
    db_path = os.path.abspath(_db_path())
    if not os.path.isfile(db_path):
        raise RuntimeError(
            f"WhatsApp message database not found at {db_path}. "
            "Start the bridge and pair a device first, or set WHATSAPP_DB_PATH."
        )
    conn = sqlite3.connect(_ro_uri(db_path), uri=True)
    try:
        wa_path = os.path.join(os.path.dirname(db_path), "whatsapp.db")
        if os.path.isfile(wa_path):
            try:
                conn.execute("ATTACH DATABASE ? AS wa", (_ro_uri(wa_path),))
            except sqlite3.Error:
                logger.debug("could not attach %s", wa_path, exc_info=True)
        yield conn
    finally:
        conn.close()


def _lid_map_available(conn: sqlite3.Connection) -> bool:
    """True when the attached whatsapp.db exposes whatsmeow_lid_map."""
    try:
        row = conn.execute(
            "SELECT 1 FROM wa.sqlite_master WHERE type = 'table' AND name = 'whatsmeow_lid_map'"
        ).fetchone()
    except sqlite3.Error:
        return False
    return row is not None


# Matches unmerged LID chats whose phone number (bare digits in whatsmeow_lid_map)
# contains the query.
_LID_PN_CLAUSE = "{jid} IN (SELECT m.lid || '@lid' FROM wa.whatsmeow_lid_map m WHERE m.pn LIKE ?)"


# Newest message of a chat: ties on timestamp go to the higher rowid. idx_messages_chat_time
# serves both the chat filter and the order. Used instead of joining on last_message_time.
_LAST_ROWID = (
    "(SELECT {a}.rowid FROM messages AS {a} WHERE {a}.chat_jid = {jid} "
    "ORDER BY {a}.timestamp DESC, {a}.rowid DESC LIMIT 1)"
)


def _user_part(jid: str) -> str:
    """Return the user part of a JID or phone number (drops `@server`)."""
    return jid.split("@")[0]


def to_jsonable(obj: Any) -> Any:
    """Convert dataclasses (recursively) to plain dicts with datetimes as ISO-8601 strings."""
    if hasattr(obj, "__dataclass_fields__"):
        obj = asdict(obj)
    if isinstance(obj, dict):
        return {k: to_jsonable(v) for k, v in obj.items()}
    if isinstance(obj, (list, tuple)):
        return [to_jsonable(v) for v in obj]
    if isinstance(obj, datetime):
        return obj.isoformat()
    return obj


@dataclass
class Message:
    timestamp: datetime
    sender: str
    content: str
    is_from_me: bool
    chat_jid: str
    id: str
    chat_name: str | None = None
    media_type: str | None = None


@dataclass
class Chat:
    jid: str
    name: str | None
    last_message_time: datetime | None
    last_message: str | None = None
    last_sender: str | None = None
    last_is_from_me: bool | None = None

    @property
    def is_group(self) -> bool:
        """Determine if chat is a group based on JID pattern."""
        return self.jid.endswith("@g.us")


@dataclass
class Contact:
    phone_number: str
    name: str | None
    jid: str


@dataclass
class MessageContext:
    message: Message
    before: list[Message]
    after: list[Message]


def _name_resolver(conn: sqlite3.Connection) -> Callable[[str], str]:
    """Return a sender -> display name lookup bound to one open connection.

    The stored sender is the user part of a JID. Its `@server` suffix is dropped and
    `user@s.whatsapp.net`, then `user@lid`, are looked up by primary key. There is no
    substring match, so `15551212` never takes the name of `551212`. Results are memoized
    for the life of the resolver; a sender with no named chat, or whose lookup fails, resolves
    to itself.
    """
    cache: dict[str, str] = {}

    def resolve(sender: str) -> str:
        if sender in cache:
            return cache[sender]
        user = _user_part(sender)
        try:
            for jid in (f"{user}@s.whatsapp.net", f"{user}@lid"):
                row = conn.execute("SELECT name FROM chats WHERE jid = ?", (jid,)).fetchone()
                if row and row[0]:
                    cache[sender] = row[0]
                    return row[0]
        except sqlite3.Error as e:
            logger.error("Database error while getting sender name: %s", e)
        cache[sender] = sender
        return sender

    return resolve


def get_sender_name(sender_jid: str) -> str:
    try:
        with _connect() as conn:
            return _name_resolver(conn)(sender_jid)
    except sqlite3.Error as e:
        logger.error("Database error while getting sender name: %s", e)
        return sender_jid


def format_message(
    message: Message,
    show_chat_info: bool = True,
    resolve_name: Callable[[str], str] | None = None,
) -> str:
    """Print a single message with consistent formatting.

    resolve_name reuses an open connection; without it the sender lookup opens its own.
    """
    output = ""

    if show_chat_info and message.chat_name:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] Chat: {message.chat_name} "
    else:
        output += f"[{message.timestamp:%Y-%m-%d %H:%M:%S}] "

    content_prefix = ""
    if hasattr(message, "media_type") and message.media_type:
        content_prefix = (
            f"[{message.media_type} - Message ID: {message.id} - Chat JID: {message.chat_jid}] "
        )

    try:
        lookup = resolve_name or get_sender_name
        sender_name = lookup(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        logger.error("Error formatting message: %s", e)
    return output


def format_messages_list(
    messages: list[Message],
    show_chat_info: bool = True,
    resolve_name: Callable[[str], str] | None = None,
) -> str:
    output = ""
    if not messages:
        output += "No messages to display."
        return output

    for message in messages:
        output += format_message(message, show_chat_info, resolve_name)
    return output


def _format_bound(value: datetime) -> str:
    """Format a time bound like the bridge driver stores timestamps.

    go-sqlite3 writes `2006-01-02 15:04:05.999999999-07:00` in the bridge's local
    zone: trailing fractional zeros dropped, numeric offset, no `Z`. Comparison is
    lexicographic on that text. A naive whole-second bound such as
    `2024-06-01 12:00:00` is a prefix of a stored value, which is why it matched
    before; bounds carrying an offset or a fraction missed rows. Order matches
    chronology only for rows that share an offset: a DST fall-back hour can
    disorder rows written across the transition. Rewriting stored values to UTC
    is out of scope.
    """
    local = value.astimezone()  # naive values are read as local time
    text = local.strftime("%Y-%m-%d %H:%M:%S")
    if local.microsecond:
        text += f".{local.microsecond:06d}".rstrip("0")
    offset = local.strftime("%z")
    return f"{text}{offset[:3]}:{offset[3:5]}"


# Trigram FTS cannot index a string shorter than 3 characters.
FTS_MIN_QUERY_LEN = 3
_FTS_CLAUSE = "messages.rowid IN (SELECT rowid FROM messages_fts WHERE messages_fts MATCH ?)"
_LIKE_CLAUSE = "LOWER(messages.content) LIKE LOWER(?) ESCAPE '\\'"


def _fts_available(conn: sqlite3.Connection) -> bool:
    """True when the bridge has built the messages_fts index and its three triggers exist.

    Without a trigger the index misses writes, so search must use LIKE instead.
    """
    row = conn.execute(
        "SELECT count(*) FROM sqlite_master WHERE name IN ('messages_fts', "
        "'messages_fts_insert', 'messages_fts_delete', 'messages_fts_update')"
    ).fetchone()
    return row[0] == 4


def _fts_phrase(text: str) -> str:
    """Quote user text as a single FTS5 phrase (internal quotes doubled)."""
    return '"' + text.replace('"', '""') + '"'


def _like_escape(text: str) -> str:
    """Escape LIKE wildcards so % and _ match only themselves (pair with ESCAPE backslash)."""
    return text.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")


def list_messages(
    after: str | None = None,
    before: str | None = None,
    sender_phone_number: str | None = None,
    chat_jid: str | None = None,
    query: str | None = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1,
) -> str:
    """Get messages matching the specified criteria with optional context."""
    try:
        with _connect() as conn:
            # One read transaction: every query below sees the same WAL snapshot, so a
            # bridge LID -> PN merge cannot move a hit between the listing and its context.
            conn.execute("BEGIN")
            cursor = conn.cursor()

            # Build base query
            query_parts = [
                "SELECT messages.timestamp, messages.sender, chats.name, messages.content, "
                "messages.is_from_me, chats.jid, messages.id, messages.media_type FROM messages"
            ]
            query_parts.append("JOIN chats ON messages.chat_jid = chats.jid")
            where_clauses = []
            params = []

            # Add filters
            if after:
                try:
                    after = datetime.fromisoformat(after)
                except ValueError as e:
                    raise ValueError(
                        f"Invalid date format for 'after': {after}. Please use ISO-8601 format."
                    ) from e

                where_clauses.append("messages.timestamp > ?")
                params.append(_format_bound(after))

            if before:
                try:
                    before = datetime.fromisoformat(before)
                except ValueError as e:
                    raise ValueError(
                        f"Invalid date format for 'before': {before}. Please use ISO-8601 format."
                    ) from e

                where_clauses.append("messages.timestamp < ?")
                params.append(_format_bound(before))

            if sender_phone_number:
                where_clauses.append("messages.sender = ?")
                params.append(sender_phone_number)

            if chat_jid:
                where_clauses.append("messages.chat_jid = ?")
                params.append(chat_jid)

            offset = page * limit

            def fetch(text_clause: str | None = None, text_param: str | None = None) -> list:
                clauses = list(where_clauses)
                bound = list(params)
                if text_clause:
                    clauses.append(text_clause)
                    bound.append(text_param)
                parts = list(query_parts)
                if clauses:
                    parts.append("WHERE " + " AND ".join(clauses))
                # Add pagination
                parts.append("ORDER BY messages.timestamp DESC")
                parts.append("LIMIT ? OFFSET ?")
                bound.extend([limit, offset])
                cursor.execute(" ".join(parts), tuple(bound))
                return cursor.fetchall()

            messages = None
            if query and len(query) >= FTS_MIN_QUERY_LEN and _fts_available(conn):
                try:
                    messages = fetch(_FTS_CLAUSE, _fts_phrase(query))
                except sqlite3.Error as e:
                    logger.warning("FTS search failed, falling back to LIKE: %s", e)
            if messages is None:
                if query:
                    messages = fetch(_LIKE_CLAUSE, f"%{_like_escape(query)}%")
                else:
                    messages = fetch()

            resolve_name = _name_resolver(conn)
            result = []
            for msg in messages:
                message = Message(
                    timestamp=datetime.fromisoformat(msg[0]),
                    sender=msg[1],
                    chat_name=msg[2],
                    content=msg[3],
                    is_from_me=msg[4],
                    chat_jid=msg[5],
                    id=msg[6],
                    media_type=msg[7],
                )
                result.append(message)

            if include_context and result:
                # Add context for each message
                messages_with_context = []
                for msg in result:
                    context = _message_context(
                        conn, msg.id, context_before, context_after, chat_jid=msg.chat_jid
                    )
                    messages_with_context.extend(context.before)
                    messages_with_context.append(context.message)
                    messages_with_context.extend(context.after)

                return format_messages_list(
                    messages_with_context, show_chat_info=True, resolve_name=resolve_name
                )

            # Format and display messages without context
            return format_messages_list(result, show_chat_info=True, resolve_name=resolve_name)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return format_messages_list([])


def get_message_context(message_id: str, before: int = 5, after: int = 5) -> MessageContext:
    """Get context around a specific message."""
    try:
        with _connect() as conn:
            return _message_context(conn, message_id, before, after)
    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise


def _message_context(
    conn: sqlite3.Connection,
    message_id: str,
    before: int,
    after: int,
    chat_jid: str | None = None,
) -> MessageContext:
    """Context around a message on an already open connection.

    The same id can exist in two chats; chat_jid picks the right one. Without it the
    newest row wins (timestamp, then rowid).
    """
    cursor = conn.cursor()

    # Get the target message first
    target_sql = """
        SELECT messages.timestamp, messages.sender, chats.name, messages.content,
               messages.is_from_me, chats.jid, messages.id, messages.chat_jid,
               messages.media_type
        FROM messages
        JOIN chats ON messages.chat_jid = chats.jid
        WHERE messages.id = ?
    """
    target_params: tuple = (message_id,)
    if chat_jid is not None:
        target_sql += " AND messages.chat_jid = ?"
        target_params += (chat_jid,)
    target_sql += " ORDER BY messages.timestamp DESC, messages.rowid DESC LIMIT 1"
    cursor.execute(target_sql, target_params)
    msg_data = cursor.fetchone()

    if not msg_data:
        raise ValueError(f"Message with ID {message_id} not found")

    target_message = Message(
        timestamp=datetime.fromisoformat(msg_data[0]),
        sender=msg_data[1],
        chat_name=msg_data[2],
        content=msg_data[3],
        is_from_me=msg_data[4],
        chat_jid=msg_data[5],
        id=msg_data[6],
        media_type=msg_data[8],
    )

    # Get messages before
    cursor.execute(
        """
        SELECT messages.timestamp, messages.sender, chats.name, messages.content,
               messages.is_from_me, chats.jid, messages.id, messages.media_type
        FROM messages
        JOIN chats ON messages.chat_jid = chats.jid
        WHERE messages.chat_jid = ? AND messages.timestamp < ?
        ORDER BY messages.timestamp DESC
        LIMIT ?
    """,
        (msg_data[7], msg_data[0], before),
    )

    before_messages = []
    for msg in cursor.fetchall():
        before_messages.append(
            Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7],
            )
        )

    # Get messages after
    cursor.execute(
        """
        SELECT messages.timestamp, messages.sender, chats.name, messages.content,
               messages.is_from_me, chats.jid, messages.id, messages.media_type
        FROM messages
        JOIN chats ON messages.chat_jid = chats.jid
        WHERE messages.chat_jid = ? AND messages.timestamp > ?
        ORDER BY messages.timestamp ASC
        LIMIT ?
    """,
        (msg_data[7], msg_data[0], after),
    )

    after_messages = []
    for msg in cursor.fetchall():
        after_messages.append(
            Message(
                timestamp=datetime.fromisoformat(msg[0]),
                sender=msg[1],
                chat_name=msg[2],
                content=msg[3],
                is_from_me=msg[4],
                chat_jid=msg[5],
                id=msg[6],
                media_type=msg[7],
            )
        )

    return MessageContext(message=target_message, before=before_messages, after=after_messages)


def list_chats(
    query: str | None = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active",
) -> list[Chat]:
    """Get chats matching the specified criteria."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            # Build base query
            query_parts = [
                """
                SELECT 
                    chats.jid,
                    chats.name,
                    chats.last_message_time,
                    {last_message_cols}
                FROM chats
            """.format(
                    last_message_cols=(
                        "messages.content as last_message, messages.sender as last_sender, "
                        "messages.is_from_me as last_is_from_me"
                        if include_last_message
                        else "NULL as last_message, NULL as last_sender, NULL as last_is_from_me"
                    )
                )
            ]

            if include_last_message:
                query_parts.append(
                    "LEFT JOIN messages ON messages.rowid = "
                    + _LAST_ROWID.format(a="m2", jid="chats.jid")
                )

            where_clauses = []
            params = []

            if query:
                clause = "LOWER(chats.name) LIKE LOWER(?) OR chats.jid LIKE ?"
                params.extend([f"%{query}%", f"%{query}%"])
                if _lid_map_available(conn):
                    clause += " OR " + _LID_PN_CLAUSE.format(jid="chats.jid")
                    params.append(f"%{query}%")
                where_clauses.append(f"({clause})")

            if where_clauses:
                query_parts.append("WHERE " + " AND ".join(where_clauses))

            # Add sorting
            order_by = "chats.last_message_time DESC" if sort_by == "last_active" else "chats.name"
            query_parts.append(f"ORDER BY {order_by}")

            # Add pagination
            offset = (page) * limit
            query_parts.append("LIMIT ? OFFSET ?")
            params.extend([limit, offset])

            cursor.execute(" ".join(query_parts), tuple(params))
            chats = cursor.fetchall()

            result = []
            for chat_data in chats:
                chat = Chat(
                    jid=chat_data[0],
                    name=chat_data[1],
                    last_message_time=datetime.fromisoformat(chat_data[2])
                    if chat_data[2]
                    else None,
                    last_message=chat_data[3],
                    last_sender=chat_data[4],
                    last_is_from_me=chat_data[5],
                )
                result.append(chat)

            return result

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []


def search_contacts(query: str) -> list[Contact]:
    """Search contacts by name or phone number."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            # Split query into characters to support partial matching
            search_pattern = "%" + query + "%"

            match = "LOWER(name) LIKE LOWER(?) OR LOWER(jid) LIKE LOWER(?)"
            params = [search_pattern, search_pattern]
            if _lid_map_available(conn):
                match += " OR " + _LID_PN_CLAUSE.format(jid="jid")
                params.append(search_pattern)

            cursor.execute(
                f"""
                SELECT DISTINCT 
                    jid,
                    name
                FROM chats
                WHERE 
                    ({match})
                    AND jid NOT LIKE '%@g.us'
                ORDER BY name, jid
                LIMIT 50
            """,
                tuple(params),
            )

            contacts = cursor.fetchall()

            result = []
            for contact_data in contacts:
                contact = Contact(
                    phone_number=contact_data[0].split("@")[0],
                    name=contact_data[1],
                    jid=contact_data[0],
                )
                result.append(contact)

            return result

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []


def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> list[Chat]:
    """Get all chats involving the contact.

    Args:
        jid: The contact's JID or bare sender (user part) to search for. Any other JID (a
            group, for example) matches its own chat exactly.
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    user = _user_part(jid)
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            # Chat ids first (direct chat by JID or the exact JID given, other chats by sender
            # index), then one row per chat with its newest message attached.
            cursor.execute(
                f"""
                SELECT
                    c.jid,
                    c.name,
                    c.last_message_time,
                    m.content as last_message,
                    m.sender as last_sender,
                    m.is_from_me as last_is_from_me
                FROM chats c
                LEFT JOIN messages m ON m.rowid = {_LAST_ROWID.format(a="m2", jid="c.jid")}
                WHERE c.jid IN (
                    SELECT jid FROM chats WHERE jid IN (? || '@s.whatsapp.net', ? || '@lid', ?)
                    UNION
                    SELECT chat_jid FROM messages WHERE sender = ?
                )
                ORDER BY c.last_message_time DESC
                LIMIT ? OFFSET ?
            """,
                (user, user, jid, user, limit, page * limit),
            )

            chats = cursor.fetchall()

            result = []
            for chat_data in chats:
                chat = Chat(
                    jid=chat_data[0],
                    name=chat_data[1],
                    last_message_time=datetime.fromisoformat(chat_data[2])
                    if chat_data[2]
                    else None,
                    last_message=chat_data[3],
                    last_sender=chat_data[4],
                    last_is_from_me=chat_data[5],
                )
                result.append(chat)

            return result

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return []


_INTERACTION_COLS = (
    "m.timestamp AS ts, m.sender, c.name, m.content, m.is_from_me, c.jid, m.id, m.media_type, "
    "m.rowid AS rid"
)

# Newest message by this sender (idx_messages_sender_time) and newest message of each direct
# chat JID form and of the exact JID given, e.g. a group (idx_messages_chat_time), then the
# newest of the four.
_LAST_INTERACTION_SQL = f"""
    SELECT * FROM (
        SELECT {_INTERACTION_COLS} FROM messages m JOIN chats c ON m.chat_jid = c.jid
        WHERE m.sender = ? ORDER BY m.timestamp DESC, m.rowid DESC LIMIT 1
    )
    UNION ALL
    SELECT * FROM (
        SELECT {_INTERACTION_COLS} FROM messages m JOIN chats c ON m.chat_jid = c.jid
        WHERE m.chat_jid = ? || '@s.whatsapp.net' ORDER BY m.timestamp DESC, m.rowid DESC LIMIT 1
    )
    UNION ALL
    SELECT * FROM (
        SELECT {_INTERACTION_COLS} FROM messages m JOIN chats c ON m.chat_jid = c.jid
        WHERE m.chat_jid = ? || '@lid' ORDER BY m.timestamp DESC, m.rowid DESC LIMIT 1
    )
    UNION ALL
    SELECT * FROM (
        SELECT {_INTERACTION_COLS} FROM messages m JOIN chats c ON m.chat_jid = c.jid
        WHERE m.chat_jid = ? ORDER BY m.timestamp DESC, m.rowid DESC LIMIT 1
    )
    ORDER BY ts DESC, rid DESC
    LIMIT 1
"""


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    user = _user_part(jid)
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            cursor.execute(_LAST_INTERACTION_SQL, (user, user, user, jid))

            msg_data = cursor.fetchone()

            if not msg_data:
                return None

            message = Message(
                timestamp=datetime.fromisoformat(msg_data[0]),
                sender=msg_data[1],
                chat_name=msg_data[2],
                content=msg_data[3],
                is_from_me=msg_data[4],
                chat_jid=msg_data[5],
                id=msg_data[6],
                media_type=msg_data[7],
            )

            return format_message(message, resolve_name=_name_resolver(conn))

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None


def get_chat(chat_jid: str, include_last_message: bool = True) -> Chat | None:
    """Get chat metadata by JID."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            last_cols = (
                "m.content as last_message, m.sender as last_sender, "
                "m.is_from_me as last_is_from_me"
                if include_last_message
                else "NULL as last_message, NULL as last_sender, NULL as last_is_from_me"
            )
            query = f"""
                SELECT 
                    c.jid,
                    c.name,
                    c.last_message_time,
                    {last_cols}
                FROM chats c
            """

            if include_last_message:
                query += " LEFT JOIN messages m ON m.rowid = " + _LAST_ROWID.format(
                    a="m2", jid="c.jid"
                )

            query += " WHERE c.jid = ?"

            cursor.execute(query, (chat_jid,))
            chat_data = cursor.fetchone()

            if not chat_data:
                return None

            return Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5],
            )

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None


def get_direct_chat_by_contact(sender_phone_number: str) -> Chat | None:
    """Get chat metadata by sender phone number.

    Matches `user@s.whatsapp.net` and `user@lid` exactly (after dropping `+`, spaces and
    dashes). The `@s.whatsapp.net` row wins when both exist.
    """
    user = _user_part(sender_phone_number.replace("+", "").replace(" ", "").replace("-", ""))
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            cursor.execute(
                f"""
                SELECT
                    c.jid,
                    c.name,
                    c.last_message_time,
                    m.content as last_message,
                    m.sender as last_sender,
                    m.is_from_me as last_is_from_me
                FROM chats c
                LEFT JOIN messages m ON m.rowid = {_LAST_ROWID.format(a="m2", jid="c.jid")}
                WHERE c.jid IN (? || '@s.whatsapp.net', ? || '@lid')
                    AND c.jid NOT LIKE '%@g.us'
                ORDER BY CASE WHEN c.jid LIKE '%@s.whatsapp.net' THEN 0 ELSE 1 END
                LIMIT 1
            """,
                (user, user),
            )
            chat_data = cursor.fetchone()

            if not chat_data:
                return None

            return Chat(
                jid=chat_data[0],
                name=chat_data[1],
                last_message_time=datetime.fromisoformat(chat_data[2]) if chat_data[2] else None,
                last_message=chat_data[3],
                last_sender=chat_data[4],
                last_is_from_me=chat_data[5],
            )

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return None


def _post(endpoint: str, payload: dict) -> tuple[dict | None, str]:
    """POST JSON to the bridge. Returns (parsed JSON body, "") or (None, error message)."""
    headers = {"Content-Type": "application/json"}
    token = _bridge_token()
    if token:
        headers["Authorization"] = f"Bearer {token}"
    try:
        with httpx.Client(timeout=HTTP_TIMEOUT, transport=_http_transport) as client:
            response = client.post(f"{_api_base_url()}/{endpoint}", json=payload, headers=headers)
    except httpx.HTTPError as e:
        return None, f"Request error: {str(e) or type(e).__name__}"
    except Exception as e:
        return None, f"Unexpected error: {str(e)}"

    if response.status_code != 200:
        return None, f"Error: HTTP {response.status_code} - {response.text}"
    try:
        result = response.json()
    except ValueError:
        return None, f"Error parsing response: {response.text}"
    if not isinstance(result, dict):
        return None, f"Error parsing response: {response.text}"
    return result, ""


def _send(payload: dict) -> tuple[bool, str]:
    """POST a send request to the bridge and return (success, message)."""
    result, error = _post("send", payload)
    if result is None:
        return False, error
    return result.get("success", False), result.get("message", "Unknown response")


def send_message(recipient: str, message: str) -> tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"
    return _send({"recipient": recipient, "message": message})


def send_file(recipient: str, media_path: str) -> tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"

    if not media_path:
        return False, "Media path must be provided"

    if not os.path.isfile(media_path):
        return False, f"Media file not found: {media_path}"

    # The bridge only accepts absolute paths.
    return _send({"recipient": recipient, "media_path": os.path.abspath(media_path)})


def send_audio_message(recipient: str, media_path: str) -> tuple[bool, str]:
    if not recipient:
        return False, "Recipient must be provided"

    if not media_path:
        return False, "Media path must be provided"

    if not os.path.isfile(media_path):
        return False, f"Media file not found: {media_path}"

    converted_path: str | None = None
    if not media_path.endswith(".ogg"):
        try:
            converted_path = audio.convert_to_opus_ogg_temp(media_path)
        except Exception as e:
            return False, f"Error converting file to opus ogg: {e}"
        media_path = converted_path

    try:
        return _send({"recipient": recipient, "media_path": os.path.abspath(media_path)})
    finally:
        # Remove the temp file we created; never touch the caller's own file.
        if converted_path:
            try:
                os.unlink(converted_path)
            except OSError:
                logger.warning("could not remove temp file %s", converted_path)


def download_media(message_id: str, chat_jid: str) -> str | None:
    """Download media from a message and return the local file path.

    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message

    Returns:
        The local file path if download was successful, None otherwise
    """
    result, error = _post("download", {"message_id": message_id, "chat_jid": chat_jid})
    if result is None:
        logger.error("%s", error)
        return None
    if result.get("success", False):
        return result.get("path")
    logger.error("Download failed: %s", result.get("message", "Unknown error"))
    return None
