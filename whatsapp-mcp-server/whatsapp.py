import logging
import os.path
import sqlite3
from collections.abc import Iterator
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
    conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
    try:
        wa_path = os.path.join(os.path.dirname(db_path), "whatsapp.db")
        if os.path.isfile(wa_path):
            try:
                conn.execute("ATTACH DATABASE ? AS wa", (f"file:{wa_path}?mode=ro",))
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


def get_sender_name(sender_jid: str) -> str:
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            # First try matching by exact JID
            cursor.execute(
                """
                SELECT name
                FROM chats
                WHERE jid = ?
                LIMIT 1
            """,
                (sender_jid,),
            )

            result = cursor.fetchone()

            # If no result, try looking for the number within JIDs
            if not result:
                # Extract the phone number part if it's a JID
                if "@" in sender_jid:
                    phone_part = sender_jid.split("@")[0]
                else:
                    phone_part = sender_jid

                cursor.execute(
                    """
                    SELECT name
                    FROM chats
                    WHERE jid LIKE ?
                    LIMIT 1
                """,
                    (f"%{phone_part}%",),
                )

                result = cursor.fetchone()

            if result and result[0]:
                return result[0]
            else:
                return sender_jid

    except sqlite3.Error as e:
        logger.error("Database error while getting sender name: %s", e)
        return sender_jid


def format_message(message: Message, show_chat_info: bool = True) -> str:
    """Print a single message with consistent formatting."""
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
        sender_name = get_sender_name(message.sender) if not message.is_from_me else "Me"
        output += f"From: {sender_name}: {content_prefix}{message.content}\n"
    except Exception as e:
        logger.error("Error formatting message: %s", e)
    return output


def format_messages_list(messages: list[Message], show_chat_info: bool = True) -> str:
    output = ""
    if not messages:
        output += "No messages to display."
        return output

    for message in messages:
        output += format_message(message, show_chat_info)
    return output


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
                params.append(after.isoformat(" "))

            if before:
                try:
                    before = datetime.fromisoformat(before)
                except ValueError as e:
                    raise ValueError(
                        f"Invalid date format for 'before': {before}. Please use ISO-8601 format."
                    ) from e

                where_clauses.append("messages.timestamp < ?")
                params.append(before.isoformat(" "))

            if sender_phone_number:
                where_clauses.append("messages.sender = ?")
                params.append(sender_phone_number)

            if chat_jid:
                where_clauses.append("messages.chat_jid = ?")
                params.append(chat_jid)

            if query:
                where_clauses.append("LOWER(messages.content) LIKE LOWER(?)")
                params.append(f"%{query}%")

            if where_clauses:
                query_parts.append("WHERE " + " AND ".join(where_clauses))

            # Add pagination
            offset = page * limit
            query_parts.append("ORDER BY messages.timestamp DESC")
            query_parts.append("LIMIT ? OFFSET ?")
            params.extend([limit, offset])

            cursor.execute(" ".join(query_parts), tuple(params))
            messages = cursor.fetchall()

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
                    context = get_message_context(msg.id, context_before, context_after)
                    messages_with_context.extend(context.before)
                    messages_with_context.append(context.message)
                    messages_with_context.extend(context.after)

                return format_messages_list(messages_with_context, show_chat_info=True)

            # Format and display messages without context
            return format_messages_list(result, show_chat_info=True)

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        return format_messages_list([])


def get_message_context(message_id: str, before: int = 5, after: int = 5) -> MessageContext:
    """Get context around a specific message."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            # Get the target message first
            cursor.execute(
                """
                SELECT messages.timestamp, messages.sender, chats.name, messages.content,
                       messages.is_from_me, chats.jid, messages.id, messages.chat_jid,
                       messages.media_type
                FROM messages
                JOIN chats ON messages.chat_jid = chats.jid
                WHERE messages.id = ?
            """,
                (message_id,),
            )
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

            return MessageContext(
                message=target_message, before=before_messages, after=after_messages
            )

    except sqlite3.Error as e:
        logger.error("Database error: %s", e)
        raise


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
                query_parts.append("""
                    LEFT JOIN messages ON chats.jid = messages.chat_jid 
                    AND chats.last_message_time = messages.timestamp
                """)

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
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            cursor.execute(
                """
                SELECT DISTINCT
                    c.jid,
                    c.name,
                    c.last_message_time,
                    m.content as last_message,
                    m.sender as last_sender,
                    m.is_from_me as last_is_from_me
                FROM chats c
                JOIN messages m ON c.jid = m.chat_jid
                WHERE m.sender = ? OR c.jid = ?
                ORDER BY c.last_message_time DESC
                LIMIT ? OFFSET ?
            """,
                (jid, jid, limit, page * limit),
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


def get_last_interaction(jid: str) -> str:
    """Get most recent message involving the contact."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            cursor.execute(
                """
                SELECT 
                    m.timestamp,
                    m.sender,
                    c.name,
                    m.content,
                    m.is_from_me,
                    c.jid,
                    m.id,
                    m.media_type
                FROM messages m
                JOIN chats c ON m.chat_jid = c.jid
                WHERE m.sender = ? OR c.jid = ?
                ORDER BY m.timestamp DESC
                LIMIT 1
            """,
                (jid, jid),
            )

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

            return format_message(message)

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
                query += """
                    LEFT JOIN messages m ON c.jid = m.chat_jid 
                    AND c.last_message_time = m.timestamp
                """

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
    """Get chat metadata by sender phone number."""
    try:
        with _connect() as conn:
            cursor = conn.cursor()

            cursor.execute(
                """
                SELECT 
                    c.jid,
                    c.name,
                    c.last_message_time,
                    m.content as last_message,
                    m.sender as last_sender,
                    m.is_from_me as last_is_from_me
                FROM chats c
                LEFT JOIN messages m ON c.jid = m.chat_jid 
                    AND c.last_message_time = m.timestamp
                WHERE c.jid LIKE ? AND c.jid NOT LIKE '%@g.us'
                LIMIT 1
            """,
                (f"%{sender_phone_number}%",),
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
