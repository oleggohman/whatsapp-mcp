import asyncio
import io
import logging
import os
import sys
import time
from contextlib import asynccontextmanager
from typing import List, Dict, Any, Optional
import qrcode
import qrcode.image.pure
from mcp.server.fastmcp import FastMCP, Image
import bridge_manager
from bridge_manager import BridgeError
from whatsapp import (
    to_jsonable,
    search_contacts as whatsapp_search_contacts,
    list_messages as whatsapp_list_messages,
    list_chats as whatsapp_list_chats,
    get_chat as whatsapp_get_chat,
    get_direct_chat_by_contact as whatsapp_get_direct_chat_by_contact,
    get_contact_chats as whatsapp_get_contact_chats,
    get_last_interaction as whatsapp_get_last_interaction,
    get_message_context as whatsapp_get_message_context,
    send_message as whatsapp_send_message,
    send_file as whatsapp_send_file,
    send_audio_message as whatsapp_audio_voice_message,
    download_media as whatsapp_download_media
)

logger = logging.getLogger(__name__)

# Bounded wait for bridge-dependent tools while the supervisor builds/starts the bridge.
BRIDGE_WAIT_SECONDS = 10.0
NOT_PAIRED_MESSAGE = "WhatsApp is not paired - call get_pairing_qr or pair_with_phone_number first"


@asynccontextmanager
async def lifespan(server: FastMCP):
    """Start the bridge supervisor in the background and return immediately.

    The bridge is a shared daemon: leaving the lifespan deliberately does not stop it.
    """
    bridge_manager.get_manager().start_background()
    yield


# Initialize FastMCP server
mcp = FastMCP("whatsapp", lifespan=lifespan)


async def _bridge_unavailable() -> Optional[str]:
    """Wait (bounded) for the bridge; return a user-facing message when it is not usable yet."""
    state = await asyncio.to_thread(bridge_manager.get_manager().wait_ready, BRIDGE_WAIT_SECONDS)
    if state == "running":
        return None
    if state == "failed":
        error = bridge_manager.get_manager().error or "unknown error"
        return f"WhatsApp bridge failed to start: {error} (see get_connection_status)"
    return "WhatsApp bridge is building/starting - retry shortly (see get_connection_status)"


def _not_paired() -> bool:
    """True when the bridge reports it is not logged in (any other outcome: False)."""
    try:
        return bridge_manager.get_manager().api("GET", "status").get("logged_in") is False
    except BridgeError:
        return False


async def _send_checked(fn, *args) -> Dict[str, Any]:
    """Run a blocking bridge send; map "not paired" failures to an actionable message."""
    unavailable = await _bridge_unavailable()
    if unavailable:
        return {"success": False, "message": unavailable}
    success, status_message = await asyncio.to_thread(fn, *args)
    if not success and await asyncio.to_thread(_not_paired):
        status_message = NOT_PAIRED_MESSAGE
    return {"success": success, "message": status_message}

@mcp.tool()
def search_contacts(query: str) -> List[Dict[str, Any]]:
    """Search WhatsApp contacts by name or phone number.
    
    Args:
        query: Search term to match against contact names or phone numbers
    """
    contacts = whatsapp_search_contacts(query)
    return to_jsonable(contacts)

@mcp.tool()
def list_messages(
    after: Optional[str] = None,
    before: Optional[str] = None,
    sender_phone_number: Optional[str] = None,
    chat_jid: Optional[str] = None,
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_context: bool = True,
    context_before: int = 1,
    context_after: int = 1
) -> str:
    """Get WhatsApp messages matching specified criteria with optional context.
    
    Args:
        after: Optional ISO-8601 formatted string to only return messages after this date
        before: Optional ISO-8601 formatted string to only return messages before this date
        sender_phone_number: Optional phone number to filter messages by sender
        chat_jid: Optional chat JID to filter messages by chat
        query: Optional search term to filter messages by content
        limit: Maximum number of messages to return (default 20)
        page: Page number for pagination (default 0)
        include_context: Whether to include messages before and after matches (default True)
        context_before: Number of messages to include before each match (default 1)
        context_after: Number of messages to include after each match (default 1)
    """
    messages = whatsapp_list_messages(
        after=after,
        before=before,
        sender_phone_number=sender_phone_number,
        chat_jid=chat_jid,
        query=query,
        limit=limit,
        page=page,
        include_context=include_context,
        context_before=context_before,
        context_after=context_after
    )
    return messages

@mcp.tool()
def list_chats(
    query: Optional[str] = None,
    limit: int = 20,
    page: int = 0,
    include_last_message: bool = True,
    sort_by: str = "last_active"
) -> List[Dict[str, Any]]:
    """Get WhatsApp chats matching specified criteria.
    
    Args:
        query: Optional search term to filter chats by name or JID
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
        include_last_message: Whether to include the last message in each chat (default True)
        sort_by: Field to sort results by, either "last_active" or "name" (default "last_active")
    """
    chats = whatsapp_list_chats(
        query=query,
        limit=limit,
        page=page,
        include_last_message=include_last_message,
        sort_by=sort_by
    )
    return to_jsonable(chats)

@mcp.tool()
def get_chat(chat_jid: str, include_last_message: bool = True) -> Optional[Dict[str, Any]]:
    """Get WhatsApp chat metadata by JID.
    
    Args:
        chat_jid: The JID of the chat to retrieve
        include_last_message: Whether to include the last message (default True)
    """
    chat = whatsapp_get_chat(chat_jid, include_last_message)
    return to_jsonable(chat)

@mcp.tool()
def get_direct_chat_by_contact(sender_phone_number: str) -> Optional[Dict[str, Any]]:
    """Get WhatsApp chat metadata by sender phone number.
    
    Args:
        sender_phone_number: The phone number to search for
    """
    chat = whatsapp_get_direct_chat_by_contact(sender_phone_number)
    return to_jsonable(chat)

@mcp.tool()
def get_contact_chats(jid: str, limit: int = 20, page: int = 0) -> List[Dict[str, Any]]:
    """Get all WhatsApp chats involving the contact.
    
    Args:
        jid: The contact's JID to search for
        limit: Maximum number of chats to return (default 20)
        page: Page number for pagination (default 0)
    """
    chats = whatsapp_get_contact_chats(jid, limit, page)
    return to_jsonable(chats)

@mcp.tool()
def get_last_interaction(jid: str) -> Optional[str]:
    """Get most recent WhatsApp message involving the contact.
    
    Args:
        jid: The JID of the contact to search for
    """
    message = whatsapp_get_last_interaction(jid)
    return message

@mcp.tool()
def get_message_context(
    message_id: str,
    before: int = 5,
    after: int = 5
) -> Dict[str, Any]:
    """Get context around a specific WhatsApp message.
    
    Args:
        message_id: The ID of the message to get context for
        before: Number of messages to include before the target message (default 5)
        after: Number of messages to include after the target message (default 5)
    """
    context = whatsapp_get_message_context(message_id, before, after)
    return to_jsonable(context)

@mcp.tool()
async def send_message(
    recipient: str,
    message: str
) -> Dict[str, Any]:
    """Send a WhatsApp message to a person or group. For group chats use the JID.

    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        message: The message text to send
    
    Returns:
        A dictionary containing success status and a status message
    """
    # Validate input
    if not recipient:
        return {
            "success": False,
            "message": "Recipient must be provided"
        }
    
    # Call the whatsapp_send_message function with the unified recipient parameter
    return await _send_checked(whatsapp_send_message, recipient, message)

@mcp.tool()
async def send_file(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send a file such as a picture, raw audio, video or document via WhatsApp to the specified recipient. For group messages use the JID.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the media file to send (image, video, document)
    
    Returns:
        A dictionary containing success status and a status message
    """
    
    # Call the whatsapp_send_file function
    return await _send_checked(whatsapp_send_file, recipient, media_path)

@mcp.tool()
async def send_audio_message(recipient: str, media_path: str) -> Dict[str, Any]:
    """Send any audio file as a WhatsApp audio message to the specified recipient. For group messages use the JID. If it errors due to ffmpeg not being installed, use send_file instead.
    
    Args:
        recipient: The recipient - either a phone number with country code but no + or other symbols,
                 or a JID (e.g., "123456789@s.whatsapp.net" or a group JID like "123456789@g.us")
        media_path: The absolute path to the audio file to send (will be converted to Opus .ogg if it's not a .ogg file)
    
    Returns:
        A dictionary containing success status and a status message
    """
    return await _send_checked(whatsapp_audio_voice_message, recipient, media_path)

@mcp.tool()
async def download_media(message_id: str, chat_jid: str) -> Dict[str, Any]:
    """Download media from a WhatsApp message and get the local file path.
    
    Args:
        message_id: The ID of the message containing the media
        chat_jid: The JID of the chat containing the message
    
    Returns:
        A dictionary containing success status, a status message, and the file path if successful
    """
    unavailable = await _bridge_unavailable()
    if unavailable:
        return {"success": False, "message": unavailable}
    file_path = await asyncio.to_thread(whatsapp_download_media, message_id, chat_jid)

    if file_path:
        return {
            "success": True,
            "message": "Media downloaded successfully",
            "file_path": file_path
        }
    else:
        return {
            "success": False,
            "message": "Failed to download media"
        }

@mcp.tool()
async def get_connection_status() -> Dict[str, Any]:
    """Get the state of the WhatsApp bridge and the WhatsApp login.

    Returns state (building, starting, running or failed), plus connected, logged_in and
    pairing (idle, qr, code, replaced) once running. When state is failed, log_tail holds the
    bridge log. restart_needed is true when the running bridge is older than the sources.
    """
    return await asyncio.to_thread(bridge_manager.get_manager().status)


def _qr_png(data: str) -> bytes:
    """Render data as a QR code PNG using the pure-Python PNG writer (no Pillow)."""
    img = qrcode.make(data, image_factory=qrcode.image.pure.PyPNGImage, box_size=8, border=4)
    buf = io.BytesIO()
    img.save(buf)
    return buf.getvalue()


def _fetch_qr() -> List[Any]:
    """Blocking part of get_pairing_qr: arm pairing if idle, then wait briefly for a code."""
    mgr = bridge_manager.get_manager()
    st = mgr.api("GET", "status")
    if st.get("logged_in"):
        return ["WhatsApp is already paired (logged in). No QR code needed."]
    if st.get("pairing") != "qr" or not st.get("qr"):
        st = mgr.api("GET", "status?start_pairing=1")
        deadline = time.monotonic() + 8.0
        while (st.get("pairing") != "qr" or not st.get("qr")) and time.monotonic() < deadline:
            if st.get("logged_in"):
                return ["WhatsApp is already paired (logged in). No QR code needed."]
            time.sleep(0.5)
            st = mgr.api("GET", "status")
    if st.get("pairing") != "qr" or not st.get("qr"):
        detail = st.get("pairing_error") or f"pairing state: {st.get('pairing')}, connected: {st.get('connected')}"
        return [f"No QR code available yet ({detail}). Call get_connection_status, then retry shortly."]
    return [
        Image(data=_qr_png(st["qr"]), format="png"),
        "Scan this QR code in WhatsApp > Linked devices > Link a device. QR codes rotate about "
        "every 20 seconds: if the scan fails, call get_pairing_qr again for a fresh code.",
        f"Raw QR string: {st['qr']}",
    ]


@mcp.tool(structured_output=False)  # returns Image + text content blocks, not a JSON payload
async def get_pairing_qr() -> List[Any]:
    """Get a QR code image to pair this server with a WhatsApp account.

    Show the image to the user immediately; they scan it in WhatsApp > Linked devices >
    Link a device. Codes rotate about every 20 seconds, so call this again if it expired.
    Starts a pairing session when none is active.
    """
    unavailable = await _bridge_unavailable()
    if unavailable:
        return [unavailable]
    try:
        return await asyncio.to_thread(_fetch_qr)
    except BridgeError as e:
        return [f"Could not get a pairing QR code: {e}"]


@mcp.tool()
async def pair_with_phone_number(phone: str) -> Dict[str, Any]:
    """Pair with WhatsApp using a phone number instead of a QR code.

    Args:
        phone: The phone number of the WhatsApp account, with country code, digits only
               (for example 14155552671)

    Returns a pairing code. The user enters it in WhatsApp > Linked devices >
    Link with phone number on that phone.
    """
    unavailable = await _bridge_unavailable()
    if unavailable:
        return {"success": False, "message": unavailable}
    try:
        body = await asyncio.to_thread(
            bridge_manager.get_manager().api, "POST", "pair-phone", json={"phone": phone}
        )
    except BridgeError as e:
        return {"success": False, "message": str(e)}
    code = body.get("code")
    if not body.get("success") or not code:
        return {"success": False, "message": body.get("message") or "Pairing failed"}
    return {
        "success": True,
        "code": code,
        "message": (
            f"Pairing code: {code}. On the phone open WhatsApp > Linked devices > Link a device > "
            "Link with phone number instead, and enter this code. Codes expire quickly."
        ),
    }


if __name__ == "__main__":
    # stdout carries the MCP stdio transport, so log to stderr only
    logging.basicConfig(
        stream=sys.stderr,
        level=os.environ.get("WHATSAPP_LOG_LEVEL", "INFO").upper(),
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    logging.getLogger("httpx").setLevel(logging.WARNING)  # one INFO line per bridge poll is noise
    # Initialize and run the server
    mcp.run(transport='stdio')