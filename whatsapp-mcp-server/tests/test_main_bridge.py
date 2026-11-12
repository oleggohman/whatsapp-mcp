"""Bridge-dependent MCP tools against a scripted manager (no bridge, no network, no real sleeps)."""

import types

import pytest
from mcp.server.fastmcp import Image

import bridge_manager
import main
from bridge_manager import BridgeError


class FakeManager:
    """Stands in for BridgeManager: scripted state, status and api answers."""

    def __init__(self, state="running", error=None):
        self.state = state
        self.error = error
        self.api_calls = []
        self.api_script = {}  # (method, path) -> list of bodies/exceptions, last one repeats
        self.status_values = []
        self.started = 0

    def start_background(self):
        self.started += 1

    def wait_ready(self, timeout):
        return self.state

    def status(self):
        return dict(self.status_values.pop(0))

    def api(self, method, path, **kwargs):
        self.api_calls.append((method, path, kwargs))
        seq = self.api_script[(method, path)]
        item = seq.pop(0) if len(seq) > 1 else seq[0]
        if isinstance(item, Exception):
            raise item
        return item


@pytest.fixture
def mgr(monkeypatch):
    fake = FakeManager()
    monkeypatch.setattr(bridge_manager, "get_manager", lambda: fake)
    return fake


@pytest.fixture
def clock(monkeypatch):
    """Fake monotonic clock that only advances when the code sleeps."""
    now = [0.0]
    sleeps = []

    def sleep(seconds):
        sleeps.append(seconds)
        now[0] += seconds

    monkeypatch.setattr(main, "time", types.SimpleNamespace(monotonic=lambda: now[0], sleep=sleep))
    return sleeps


async def test_lifespan_starts_supervisor_in_background(mgr):
    async with main.lifespan(main.mcp):
        assert mgr.started == 1


async def test_failed_bridge_message_includes_error_or_unknown(mgr):
    mgr.state, mgr.error = "failed", "go build failed"
    msg = await main._bridge_unavailable()
    assert msg == "WhatsApp bridge failed to start: go build failed (see get_connection_status)"
    mgr.error = None
    assert "failed to start: unknown error" in await main._bridge_unavailable()
    mgr.state = "building"
    assert "building/starting - retry shortly" in await main._bridge_unavailable()
    mgr.state = "running"
    assert await main._bridge_unavailable() is None


async def test_unavailable_bridge_short_circuits_every_tool(mgr):
    mgr.state = "starting"
    expected = {"success": False, "message": await main._bridge_unavailable()}
    assert await main.download_media("m1", "111@s.whatsapp.net") == expected
    assert await main.pair_with_phone_number("14155552671") == expected
    assert await main.get_pairing_qr() == [expected["message"]]
    assert mgr.api_calls == []


async def test_failed_send_reports_not_paired(mgr):
    mgr.api_script[("GET", "status")] = [{"logged_in": False}]
    assert await main._send_checked(lambda: (False, "send failed")) == {
        "success": False,
        "message": main.NOT_PAIRED_MESSAGE,
    }
    # Paired (or status unavailable) keeps the original failure message.
    mgr.api_script[("GET", "status")] = [{"logged_in": True}]
    assert (await main._send_checked(lambda: (False, "boom")))["message"] == "boom"
    mgr.api_script[("GET", "status")] = [BridgeError("down")]
    assert (await main._send_checked(lambda: (False, "boom")))["message"] == "boom"


async def test_download_media_failure_message(mgr, monkeypatch):
    monkeypatch.setattr(main, "whatsapp_download_media", lambda message_id, chat: None)
    assert await main.download_media("m1", "c") == {
        "success": False,
        "message": "Failed to download media",
    }


# --- get_connection_status ---------------------------------------------------------------------


async def test_connection_status_without_reconnect(mgr):
    mgr.status_values = [{"state": "running", "pairing": "replaced"}]
    assert await main.get_connection_status() == {"state": "running", "pairing": "replaced"}
    assert mgr.api_calls == []


async def test_reconnect_refused_when_bridge_not_running(mgr):
    mgr.status_values = [{"state": "failed"}]
    out = await main.get_connection_status(reconnect=True)
    assert out == {"state": "failed", "reconnect_error": "bridge is not running"}
    assert mgr.api_calls == []


async def test_reconnect_reports_api_error(mgr):
    mgr.status_values = [{"state": "running"}]
    mgr.api_script[("POST", "reconnect")] = [BridgeError("already connected")]
    out = await main.get_connection_status(reconnect=True)
    assert out == {"state": "running", "reconnect_error": "already connected"}


async def test_reconnect_returns_fresh_status(mgr):
    mgr.status_values = [{"state": "running", "pairing": "replaced"}, {"state": "running"}]
    mgr.api_script[("POST", "reconnect")] = [{"success": True}]
    assert await main.get_connection_status(reconnect=True) == {"state": "running"}
    assert mgr.api_calls == [("POST", "reconnect", {"json": {}})]


# --- get_pairing_qr ----------------------------------------------------------------------------

ALREADY_PAIRED = "WhatsApp is already paired (logged in). No QR code needed."


def _is_qr_answer(out, code):
    assert isinstance(out[0], Image) and out[0].data.startswith(b"\x89PNG")
    assert out[2] == f"Raw QR string: {code}"


async def test_qr_when_already_logged_in(mgr):
    mgr.api_script[("GET", "status")] = [{"logged_in": True}]
    assert await main.get_pairing_qr() == [ALREADY_PAIRED]


async def test_qr_reuses_active_pairing_session(mgr, clock):
    mgr.api_script[("GET", "status")] = [{"pairing": "qr", "qr": "CODE-1"}]
    _is_qr_answer(await main.get_pairing_qr(), "CODE-1")
    assert [c[:2] for c in mgr.api_calls] == [("GET", "status")]  # start-pairing not called
    assert clock == []


async def test_qr_started_pairing_returns_code_immediately(mgr, clock):
    mgr.api_script[("GET", "status")] = [{"pairing": "idle"}]
    mgr.api_script[("POST", "start-pairing")] = [{"pairing": "qr", "qr": "CODE-2"}]
    _is_qr_answer(await main.get_pairing_qr(), "CODE-2")
    assert clock == []


async def test_qr_polls_until_code_appears(mgr, clock):
    mgr.api_script[("GET", "status")] = [
        {"pairing": "idle"},  # initial check
        {"pairing": "starting"},
        {"pairing": "qr", "qr": "CODE-3"},
    ]
    mgr.api_script[("POST", "start-pairing")] = [{"pairing": "starting"}]
    _is_qr_answer(await main.get_pairing_qr(), "CODE-3")
    assert clock == [0.5, 0.5]


async def test_qr_poll_notices_login_completing(mgr, clock):
    mgr.api_script[("GET", "status")] = [{"pairing": "idle"}]
    mgr.api_script[("POST", "start-pairing")] = [{"pairing": "starting", "logged_in": True}]
    assert await main.get_pairing_qr() == [ALREADY_PAIRED]
    assert clock == []


async def test_qr_gives_up_after_deadline_with_pairing_error(mgr, clock):
    mgr.api_script[("GET", "status")] = [{"pairing": "idle", "pairing_error": "rate limited"}]
    mgr.api_script[("POST", "start-pairing")] = [{"pairing": "starting"}]
    out = await main.get_pairing_qr()
    assert out == [
        "No QR code available yet (rate limited). Call get_connection_status, then retry shortly."
    ]
    assert sum(clock) == pytest.approx(8.0)


async def test_qr_gives_up_after_deadline_with_state_detail(mgr, clock):
    mgr.api_script[("GET", "status")] = [{"pairing": "idle", "connected": False}]
    mgr.api_script[("POST", "start-pairing")] = [{"pairing": "starting", "connected": False}]
    (text,) = await main.get_pairing_qr()
    assert "(pairing state: idle, connected: False)" in text


async def test_qr_bridge_error_is_reported_not_raised(mgr):
    mgr.api_script[("GET", "status")] = [BridgeError("bridge request failed: ConnectError")]
    assert await main.get_pairing_qr() == [
        "Could not get a pairing QR code: bridge request failed: ConnectError"
    ]


# --- pair_with_phone_number --------------------------------------------------------------------


async def test_pair_with_phone_number_success(mgr):
    mgr.api_script[("POST", "pair-phone")] = [{"success": True, "code": "ABCD-1234"}]
    out = await main.pair_with_phone_number("14155552671")
    assert out["success"] is True and out["code"] == "ABCD-1234"
    assert "Pairing code: ABCD-1234." in out["message"]
    assert mgr.api_calls == [("POST", "pair-phone", {"json": {"phone": "14155552671"}})]


async def test_pair_with_phone_number_failures(mgr):
    mgr.api_script[("POST", "pair-phone")] = [{"success": False, "message": "bad number"}]
    assert await main.pair_with_phone_number("1") == {"success": False, "message": "bad number"}
    mgr.api_script[("POST", "pair-phone")] = [{"success": True}]  # success without a code
    assert await main.pair_with_phone_number("1") == {"success": False, "message": "Pairing failed"}
    mgr.api_script[("POST", "pair-phone")] = [BridgeError("no bridge")]
    assert await main.pair_with_phone_number("1") == {"success": False, "message": "no bridge"}
