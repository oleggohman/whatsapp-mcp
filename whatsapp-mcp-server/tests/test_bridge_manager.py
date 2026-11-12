"""Bridge supervisor: build, spawn, shared-daemon reuse and the pairing tools (fake bridge + fake go)."""

import json
import os
import signal
import socket
import stat
import subprocess
import sys
import threading
import time
from pathlib import Path

import httpx
import pytest

import bridge_manager
import main
from bridge_manager import BridgeError, BridgeManager

FAKE_BRIDGE_SRC = Path(__file__).with_name("fake_bridge.py")


def _data(result):
    """Structured payload of a call_tool result (unwrapping FastMCP's {"result": ...})."""
    structured = result[1]
    return structured["result"] if set(structured) == {"result"} else structured


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _write_exe(path: Path, text: str) -> Path:
    path.write_text(text)
    path.chmod(path.stat().st_mode | stat.S_IXUSR)
    return path


@pytest.fixture
def env(tmp_path, monkeypatch):
    """Autostart enabled, private store/port, fake bridge binary, empty source dir (no build)."""
    store = tmp_path / "store"
    binary = _write_exe(tmp_path / "bridge-bin", f"#!{sys.executable}\n" + FAKE_BRIDGE_SRC.read_text())
    src = tmp_path / "src"
    src.mkdir()
    port = _free_port()
    monkeypatch.setenv("WHATSAPP_BRIDGE_AUTOSTART", "1")
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(store))
    monkeypatch.setenv("WHATSAPP_API_BASE_URL", f"http://127.0.0.1:{port}/api")
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", str(binary))
    monkeypatch.setenv("WHATSAPP_BRIDGE_SRC", str(src))
    monkeypatch.setenv("WHATSAPP_BRIDGE_START_TIMEOUT", "20")
    monkeypatch.setenv("WHATSAPP_DB_PATH", str(store / "messages.db"))
    stores = [store]
    yield {"store": store, "binary": binary, "src": src, "port": port, "tmp": tmp_path, "stores": stores}
    for s in stores:  # stop every fake daemon this test started
        pid_file = s / "fake.pid"
        if pid_file.exists():
            try:
                os.kill(int(pid_file.read_text()), signal.SIGKILL)
            except (ProcessLookupError, ValueError):
                pass


def spawns(store: Path) -> int:
    log = store / "spawns.log"
    return len(log.read_text().split()) if log.exists() else 0


def start_fake(env, store: Path | None = None, **extra) -> subprocess.Popen:
    """Start the fake bridge by hand (as another MCP client would have) and wait until it answers."""
    store = store or env["store"]
    if store not in env["stores"]:
        env["stores"].append(store)
    e = dict(os.environ, WHATSAPP_STORE_DIR=str(store), WHATSAPP_BRIDGE_ADDR=f"127.0.0.1:{env['port']}", **extra)
    proc = subprocess.Popen([str(env["binary"])], env=e, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    return proc


def wait_health(env, timeout=10.0) -> dict:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            return httpx.get(f"http://127.0.0.1:{env['port']}/api/health", timeout=1).json()
        except (httpx.HTTPError, ValueError):
            time.sleep(0.1)
    raise AssertionError("fake bridge did not answer")


# --- ensure_running ----------------------------------------------------------------------------


def test_reuses_matching_bridge_without_spawn(env):
    start_fake(env)
    wait_health(env)
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running"
    assert spawns(env["store"]) == 1  # only the hand-started one


def test_rejects_bridge_with_other_store_dir(env):
    start_fake(env, store=env["tmp"] / "other-store")
    wait_health(env)
    mgr = BridgeManager()
    with pytest.raises(BridgeError, match="another bridge/store"):
        mgr.ensure_running()
    assert spawns(env["store"]) == 0


def test_spawns_and_waits_for_health(env):
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running"
    assert spawns(env["store"]) == 1
    assert (env["store"] / "bridge.log").exists()
    assert oct((env["store"] / "bridge.token").stat().st_mode & 0o777) == "0o600"
    assert mgr.api("GET", "status")["pairing"] == "idle"  # authenticated with the generated token


def test_exit_75_keeps_polling_until_peer_answers(env):
    start_fake(env, FAKE_START_DELAY="1.5")  # peer holds the lock but is not listening yet
    time.sleep(0.5)
    mgr = BridgeManager()
    mgr.ensure_running()  # our spawn exits 75; health answers once the peer is up
    assert mgr.state == "running"
    assert spawns(env["store"]) == 1


def test_early_exit_reports_log_tail(env, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT_EARLY", "3")
    mgr = BridgeManager()
    with pytest.raises(BridgeError, match="exited with code 3") as exc:
        mgr.ensure_running()
    assert "boom: fake bridge failing on purpose" in str(exc.value)


def test_two_managers_concurrently_share_one_bridge(env, monkeypatch):
    monkeypatch.setenv("FAKE_START_DELAY", "0.8")
    managers = [BridgeManager(), BridgeManager()]
    errors: list[BaseException] = []

    def run(m):
        try:
            m.ensure_running()
        except BaseException as e:  # noqa: BLE001
            errors.append(e)

    threads = [threading.Thread(target=run, args=(m,)) for m in managers]
    for t in threads:
        t.start()
    for t in threads:
        t.join(30)
    assert errors == []
    assert [m.state for m in managers] == ["running", "running"]
    assert spawns(env["store"]) == 1
    token = (env["store"] / "bridge.token").read_text().strip()
    assert all(m.api("GET", "status")["pairing"] == "idle" for m in managers)
    assert bridge_manager.read_token() == token


def test_unknown_token_is_a_clear_error(env):
    start_fake(env, FAKE_TOKEN="not-our-token")
    wait_health(env)
    mgr = BridgeManager()
    mgr.ensure_running()  # health is open, so reuse works
    with pytest.raises(BridgeError, match="unknown token"):
        mgr.api("GET", "status")
    assert "unknown token" in mgr.status()["error"]


def test_autostart_disabled(env, monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_AUTOSTART", "0")
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "idle"
    assert spawns(env["store"]) == 0
    assert mgr.wait_ready(0.1) == "running"  # manual mode never blocks tools
    status = mgr.status()
    assert status["state"] == "failed" and "autostart is disabled" in status["error"]


def test_token_created_exclusively_and_shared(env):
    results: list[str] = []
    threads = [threading.Thread(target=lambda: results.append(bridge_manager.ensure_token())) for _ in range(8)]
    for t in threads:
        t.start()
    for t in threads:
        t.join(10)
    assert len(set(results)) == 1 and len(results[0]) == 64
    assert oct((env["store"] / "bridge.token").stat().st_mode & 0o777) == "0o600"


def test_env_token_wins_over_file(env, monkeypatch):
    (env["store"]).mkdir()
    (env["store"] / "bridge.token").write_text("from-file")
    assert bridge_manager.read_token() == "from-file"
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "from-env")
    assert bridge_manager.read_token() == "from-env"


# --- build -------------------------------------------------------------------------------------


def make_fake_go(env, sleep: float = 0, fail: bool = False) -> Path:
    """A fake `go` that records its args, sleeps, then writes the fake bridge to the -o path."""
    log = env["tmp"] / "go-args.log"
    body = f"""#!/bin/sh
echo "$@" >> {log}
echo "go: noisy build output"
sleep {sleep}
"""
    if fail:
        body += 'echo "compile error: boom" >&2\nexit 2\n'
    else:
        body += f"""while [ $# -gt 0 ]; do
  if [ "$1" = "-o" ]; then out="$2"; fi
  shift
done
cp {env['binary']} "$out"
chmod +x "$out"
"""
    return _write_exe(env["tmp"] / "fake-go", body)


def seed_sources(env) -> None:
    (env["src"] / "main.go").write_text("package main\n")
    (env["src"] / "go.sum").write_text("x\n")


def test_build_is_atomic_and_captured(env, monkeypatch, capfd):
    seed_sources(env)
    target = env["tmp"] / "built-bridge"
    target.write_text("OLD-BINARY")
    old_time = time.time() - 1000
    os.utime(target, (old_time, old_time))
    monkeypatch.setenv("WHATSAPP_GO_BIN", str(make_fake_go(env, sleep=1)))

    t = threading.Thread(target=bridge_manager.build_bridge, args=(str(target), str(env["src"])))
    t.start()
    time.sleep(0.4)
    assert target.read_text() == "OLD-BINARY"  # untouched while go is still running
    t.join(20)
    assert target.read_text().startswith("#!")  # replaced in one step
    assert [p.name for p in env["tmp"].iterdir() if ".tmp." in p.name] == []
    args = (env["tmp"] / "go-args.log").read_text()
    assert "-X main.bridgeVersion=" in args and "build" in args
    assert bridge_manager.built_version(str(target)) == bridge_manager.source_hash(str(env["src"]))
    assert "noisy build output" not in capfd.readouterr().out  # never inherited stdout


def test_build_failure_keeps_old_binary_and_reports_output(env, monkeypatch):
    seed_sources(env)
    target = env["tmp"] / "built-bridge"
    target.write_text("OLD-BINARY")
    monkeypatch.setenv("WHATSAPP_GO_BIN", str(make_fake_go(env, fail=True)))
    with pytest.raises(BridgeError, match="compile error: boom"):
        bridge_manager.build_bridge(str(target), str(env["src"]))
    assert target.read_text() == "OLD-BINARY"
    assert [p.name for p in env["tmp"].iterdir() if ".tmp." in p.name] == []


def test_go_probing(env, monkeypatch, tmp_path):
    fake = _write_exe(tmp_path / "mygo", "#!/bin/sh\n")
    monkeypatch.setenv("WHATSAPP_GO_BIN", str(fake))
    assert bridge_manager.find_go() == str(fake)
    monkeypatch.delenv("WHATSAPP_GO_BIN")
    home = tmp_path / "home"
    (home / "go" / "bin").mkdir(parents=True)
    _write_exe(home / "go" / "bin" / "go", "#!/bin/sh\n")
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("PATH", str(tmp_path / "empty"))
    found = bridge_manager.find_go()
    # Real installs under /opt/homebrew etc. may win on a dev machine; the probe list is what matters.
    assert found is not None and found.endswith("/go")


def test_needs_build_by_mtime(env):
    seed_sources(env)
    binary = env["tmp"] / "b"
    assert bridge_manager.needs_build(str(binary), str(env["src"]))  # missing
    binary.write_text("x")
    future = time.time() + 100
    os.utime(binary, (future, future))
    assert not bridge_manager.needs_build(str(binary), str(env["src"]))
    old = time.time() - 100
    os.utime(binary, (old, old))
    assert bridge_manager.needs_build(str(binary), str(env["src"]))


def test_ensure_running_builds_when_binary_missing(env, monkeypatch):
    seed_sources(env)
    built = env["tmp"] / "fresh-bridge"
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", str(built))
    monkeypatch.setenv("WHATSAPP_GO_BIN", str(make_fake_go(env)))
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running" and built.exists()
    assert spawns(env["store"]) == 1


# --- non-blocking startup and tools --------------------------------------------------------------


async def test_lifespan_returns_fast_and_tools_report_starting(env, monkeypatch):
    seed_sources(env)
    built = env["tmp"] / "fresh-bridge"
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", str(built))
    monkeypatch.setenv("WHATSAPP_GO_BIN", str(make_fake_go(env, sleep=2)))
    monkeypatch.setattr(main, "BRIDGE_WAIT_SECONDS", 0.2)

    t0 = time.monotonic()
    async with main.lifespan(main.mcp):
        assert time.monotonic() - t0 < 1.0  # the 2 s build did not block startup
        payload = _data(await main.mcp.call_tool("send_message", {"recipient": "123", "message": "hi"}))
        assert payload["success"] is False and "building/starting" in payload["message"]
        status = _data(await main.mcp.call_tool("get_connection_status", {}))
        assert status["state"] in ("building", "starting")
    # Leaving the lifespan must not stop anything: the supervisor finishes and the bridge stays up.
    assert bridge_manager.get_manager()._done.wait(30)
    assert bridge_manager.get_manager().state == "running"
    assert wait_health(env)["service"] == "whatsapp-bridge"


async def test_pairing_tools_through_mcp(env):
    mgr = bridge_manager.get_manager()
    mgr.start_background()
    assert mgr._done.wait(30)

    status = _data(await main.mcp.call_tool("get_connection_status", {}))
    assert status["state"] == "running" and status["logged_in"] is False and status["pairing"] == "idle"
    assert "restart_needed" not in status

    content = await main.mcp.call_tool("get_pairing_qr", {})
    kinds = [c.type for c in content]
    assert kinds[0] == "image" and content[0].mimeType == "image/png"
    import base64
    assert base64.b64decode(content[0].data)[:8] == b"\x89PNG\r\n\x1a\n"
    assert any("2@fakeqrpayload" in getattr(c, "text", "") for c in content)

    ok = _data(await main.mcp.call_tool("pair_with_phone_number", {"phone": "+1 415 555 2671"}))
    assert ok["success"] is False  # fake bridge rejects non-digit input like the real validation
    ok = _data(await main.mcp.call_tool("pair_with_phone_number", {"phone": "14155552671"}))
    assert ok["success"] is True and ok["code"] == "ABCD-EFGH" and "Linked devices" in ok["message"]


async def test_send_when_not_logged_in_says_not_paired(env):
    mgr = bridge_manager.get_manager()
    mgr.start_background()
    assert mgr._done.wait(30)
    res = _data(await main.mcp.call_tool("send_message", {"recipient": "123", "message": "hi"}))
    assert res == {"success": False, "message": main.NOT_PAIRED_MESSAGE}


async def test_send_when_logged_in_passes_through(env, monkeypatch):
    monkeypatch.setenv("FAKE_LOGGED_IN", "1")
    mgr = bridge_manager.get_manager()
    mgr.start_background()
    assert mgr._done.wait(30)
    res = _data(await main.mcp.call_tool("send_message", {"recipient": "123", "message": "hi"}))
    assert res == {"success": True, "message": "sent"}
    qr = await main.mcp.call_tool("get_pairing_qr", {})
    assert "already paired" in qr[0].text


def test_stale_daemon_is_reported(env, monkeypatch):
    monkeypatch.setenv("FAKE_VERSION", "old-hash")
    Path(str(env["binary"]) + ".srchash").write_text("new-hash")
    mgr = BridgeManager()
    mgr.ensure_running()
    status = mgr.status()
    assert status["restart_needed"] is True and "kill" in status["hint"]


def test_failed_state_carries_log_tail(env, monkeypatch):
    monkeypatch.setenv("FAKE_EXIT_EARLY", "3")
    mgr = BridgeManager()
    mgr.start_background()
    assert mgr._done.wait(30)
    status = mgr.status()
    assert status["state"] == "failed" and "boom" in status["log_tail"]


def test_log_rotates_when_large(env, monkeypatch):
    env["store"].mkdir()
    log = env["store"] / "bridge.log"
    log.write_bytes(b"x" * (bridge_manager.LOG_ROTATE_BYTES + 1))
    BridgeManager().ensure_running()
    assert (env["store"] / "bridge.log.1").exists()
    assert log.stat().st_size < 1000
