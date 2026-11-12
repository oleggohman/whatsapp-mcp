"""status() keeps working when the bridge sources change underneath it."""

import bridge_manager
from bridge_manager import BridgeManager


def _script_health(monkeypatch, answer):
    monkeypatch.setattr(BridgeManager, "health", lambda self: answer)


def test_status_survives_sources_changing_mid_read(tmp_path, monkeypatch):
    """A checkout/pull can remove a source file between glob and open: fall back to the sidecar."""
    binary = tmp_path / "bridge-bin"
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", str(binary))
    (tmp_path / "bridge-bin.srchash").write_text("abc123")

    def vanished(src):
        raise FileNotFoundError("main.go")

    monkeypatch.setattr(bridge_manager, "source_hash", vanished)
    _script_health(monkeypatch, {"service": "whatsapp-bridge", "version": "old", "pid": 42})
    monkeypatch.setattr(BridgeManager, "api", lambda self, m, p, **kw: {"connected": True})
    out = BridgeManager().status()
    assert out["state"] == "running" and out["connected"] is True
    assert out["restart_needed"] is True  # compared against the sidecar value
    assert "Go is available" in out["hint"]

    _script_health(monkeypatch, {"service": "whatsapp-bridge", "version": "abc123"})
    assert "restart_needed" not in BridgeManager().status()
