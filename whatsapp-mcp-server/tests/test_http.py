"""Bridge HTTP helpers, exercised through httpx.MockTransport (no real network)."""

import json
import os

import httpx
import pytest

import whatsapp


@pytest.fixture
def bridge(monkeypatch):
    """Install a MockTransport; returns (requests list, setter for the handler)."""
    requests: list[httpx.Request] = []
    state = {"handler": lambda req: httpx.Response(200, json={"success": True, "message": "ok"})}

    def handler(request: httpx.Request) -> httpx.Response:
        requests.append(request)
        return state["handler"](request)

    monkeypatch.setattr(whatsapp, "_http_transport", httpx.MockTransport(handler))
    monkeypatch.delenv("WHATSAPP_BRIDGE_TOKEN", raising=False)
    monkeypatch.delenv("WHATSAPP_API_BASE_URL", raising=False)
    return requests, state


def test_send_message_success(bridge):
    requests, _ = bridge
    assert whatsapp.send_message("123", "hi") == (True, "ok")
    assert str(requests[0].url) == "http://localhost:8080/api/send"
    assert json.loads(requests[0].content) == {"recipient": "123", "message": "hi"}
    assert requests[0].headers["content-type"] == "application/json"


def test_send_message_requires_recipient(bridge):
    requests, _ = bridge
    assert whatsapp.send_message("", "hi") == (False, "Recipient must be provided")
    assert requests == []


def test_http_error(bridge):
    _, state = bridge
    state["handler"] = lambda req: httpx.Response(403, text="forbidden")
    assert whatsapp.send_message("123", "hi") == (False, "Error: HTTP 403 - forbidden")


def test_bad_json(bridge):
    _, state = bridge
    state["handler"] = lambda req: httpx.Response(200, text="not json")
    ok, msg = whatsapp.send_message("123", "hi")
    assert not ok and msg.startswith("Error parsing response")


def test_timeout(bridge):
    _, state = bridge

    def boom(req):
        raise httpx.ReadTimeout("slow", request=req)

    state["handler"] = boom
    ok, msg = whatsapp.send_message("123", "hi")
    assert not ok and msg.startswith("Request error:")


def test_connection_error(bridge):
    _, state = bridge

    def boom(req):
        raise httpx.ConnectError("refused", request=req)

    state["handler"] = boom
    ok, msg = whatsapp.send_message("123", "hi")
    assert not ok and "refused" in msg


def test_client_has_timeout():
    assert whatsapp.HTTP_TIMEOUT.read is not None and whatsapp.HTTP_TIMEOUT.connect is not None


def test_token_header_only_when_set(bridge, monkeypatch):
    requests, _ = bridge
    whatsapp.send_message("123", "hi")
    assert "authorization" not in requests[-1].headers
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "s3cret")
    whatsapp.send_message("123", "hi")
    assert requests[-1].headers["authorization"] == "Bearer s3cret"


def test_base_url_env(bridge, monkeypatch):
    requests, _ = bridge
    monkeypatch.setenv("WHATSAPP_API_BASE_URL", "http://127.0.0.1:9999/api/")
    whatsapp.send_message("123", "hi")
    assert str(requests[-1].url) == "http://127.0.0.1:9999/api/send"


def test_send_file_makes_path_absolute(bridge, tmp_path, monkeypatch):
    requests, _ = bridge
    (tmp_path / "a.txt").write_text("x")
    monkeypatch.chdir(tmp_path)
    assert whatsapp.send_file("123", "a.txt") == (True, "ok")
    sent = json.loads(requests[0].content)["media_path"]
    assert os.path.isabs(sent) and sent.endswith("a.txt")
    assert os.path.realpath(sent) == os.path.realpath(tmp_path / "a.txt")


def test_send_file_missing(bridge):
    requests, _ = bridge
    ok, msg = whatsapp.send_file("123", "/nonexistent/file.bin")
    assert not ok and "not found" in msg and requests == []


def test_send_audio_ogg_passthrough(bridge, tmp_path):
    requests, _ = bridge
    f = tmp_path / "v.ogg"
    f.write_bytes(b"OggS")
    assert whatsapp.send_audio_message("123", str(f)) == (True, "ok")
    assert json.loads(requests[0].content)["media_path"] == str(f)


def test_download_media(bridge):
    requests, state = bridge
    state["handler"] = lambda req: httpx.Response(200, json={"success": True, "path": "/x/y.jpg"})
    assert whatsapp.download_media("m1", "c@s.whatsapp.net") == "/x/y.jpg"
    assert json.loads(requests[0].content) == {"message_id": "m1", "chat_jid": "c@s.whatsapp.net"}
    assert str(requests[0].url).endswith("/api/download")
    state["handler"] = lambda req: httpx.Response(200, json={"success": False, "message": "nope"})
    assert whatsapp.download_media("m1", "c") is None
    state["handler"] = lambda req: httpx.Response(500, text="boom")
    assert whatsapp.download_media("m1", "c") is None
