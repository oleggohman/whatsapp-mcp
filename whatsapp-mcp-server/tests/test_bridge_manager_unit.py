"""Bridge supervisor edge cases driven by stubs and failure injection (no real bridge process)."""

import errno
import os
import subprocess
import threading

import httpx
import pytest

import bridge_manager
from bridge_manager import BridgeError, BridgeManager


@pytest.fixture
def autostart(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_AUTOSTART", "1")


def _script_health(monkeypatch, answers):
    """Make BridgeManager.health return the given answers in order (last one repeats)."""
    seq = list(answers)

    def health(self):
        return seq.pop(0) if len(seq) > 1 else seq[0]

    monkeypatch.setattr(BridgeManager, "health", health)


# --- configuration -----------------------------------------------------------------------------


def test_bridge_addr_explicit_and_localhost_mapping(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_ADDR", "0.0.0.0:9000")
    assert bridge_manager.bridge_addr() == "0.0.0.0:9000"
    monkeypatch.delenv("WHATSAPP_BRIDGE_ADDR")
    monkeypatch.setenv("WHATSAPP_API_BASE_URL", "http://localhost:9999/api")
    assert bridge_manager.bridge_addr() == "127.0.0.1:9999"
    monkeypatch.setenv("WHATSAPP_API_BASE_URL", "http://example.test/api")
    assert bridge_manager.bridge_addr() == "example.test:80"


def test_invalid_start_timeout_falls_back_to_default(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_START_TIMEOUT", "soon")
    assert bridge_manager.start_timeout() == 60.0
    monkeypatch.setenv("WHATSAPP_BRIDGE_START_TIMEOUT", "7.5")
    assert bridge_manager.start_timeout() == 7.5


# --- token -------------------------------------------------------------------------------------


def test_token_fallback_reports_existing_file_when_link_unsupported(tmp_path, monkeypatch):
    """No hard links and the token file already exists: the exclusive create loses cleanly."""
    path = tmp_path / "bridge.token"
    path.write_text("peer-token")
    tmp = tmp_path / "bridge.token.tmp"
    tmp.write_text("mine")

    def no_link(src, dst):
        raise OSError(errno.EPERM, "hard links unsupported")

    monkeypatch.setattr(bridge_manager.os, "link", no_link)
    assert bridge_manager._create_token_file(str(tmp), str(path), "mine") is False
    assert path.read_text() == "peer-token"


def test_recover_empty_token_without_flock_support(tmp_path, monkeypatch):
    """Platforms without fcntl (Windows) still replace a stale empty token file."""
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path))
    monkeypatch.setattr(bridge_manager, "fcntl", None)
    path = tmp_path / "bridge.token"
    path.write_text("")
    tmp = tmp_path / "bridge.token.tmp"
    tmp.write_text("fresh")
    assert bridge_manager._recover_empty_token(str(tmp), str(path), "fresh") == "fresh"
    assert path.read_text() == "fresh"


def test_recover_empty_token_reads_the_winner(tmp_path, monkeypatch):
    """A peer replaced the empty file first: its token wins and ours is not installed."""
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path))
    path = tmp_path / "bridge.token"
    path.write_text("winner")
    tmp = tmp_path / "bridge.token.tmp"
    tmp.write_text("loser")
    assert bridge_manager._recover_empty_token(str(tmp), str(path), "loser") == "winner"
    assert path.read_text() == "winner" and tmp.read_text() == "loser"


def test_ensure_token_reads_token_of_peer_that_created_it_first(tmp_path, monkeypatch):
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path))

    def peer_won(tmp, path, token):
        with open(path, "w", encoding="utf-8") as f:
            f.write("peer-token")
        return False

    monkeypatch.setattr(bridge_manager, "_create_token_file", peer_won)
    assert bridge_manager.ensure_token() == "peer-token"
    assert sorted(p.name for p in tmp_path.iterdir()) == ["bridge.token"]


# --- find_go -----------------------------------------------------------------------------------


def test_find_go_uses_path_lookup(monkeypatch):
    monkeypatch.setattr(bridge_manager.shutil, "which", lambda name: f"/usr/bin/{name}")
    assert bridge_manager.find_go() == "/usr/bin/go"


def test_find_go_probes_install_locations(tmp_path, monkeypatch):
    monkeypatch.setattr(bridge_manager.shutil, "which", lambda name: None)
    plain = tmp_path / "plain" / "go"  # present but not executable
    plain.parent.mkdir()
    plain.write_text("")
    plain.chmod(0o644)
    real = tmp_path / "sdk" / "go1.22" / "bin" / "go"
    real.parent.mkdir(parents=True)
    real.write_text("")
    real.chmod(0o755)
    patterns = (str(tmp_path / "nothing*" / "go"), str(plain), str(tmp_path / "sdk/*/bin/go"))
    monkeypatch.setattr(bridge_manager, "GO_PROBE_PATHS", patterns)
    assert bridge_manager.find_go() == str(real)

    monkeypatch.setattr(bridge_manager, "GO_PROBE_PATHS", patterns[:2])
    assert bridge_manager.find_go() is None


# --- build_bridge failure modes ----------------------------------------------------------------


@pytest.fixture
def build_env(tmp_path, monkeypatch):
    src = tmp_path / "src"
    src.mkdir()
    (src / "main.go").write_text("package main\n")
    monkeypatch.setattr(bridge_manager, "find_go", lambda: "/fake/go")
    return str(tmp_path / "out" / "bridge"), str(src)


def test_build_without_go_toolchain_is_a_clear_error(build_env, monkeypatch):
    binary, src = build_env
    monkeypatch.setattr(bridge_manager, "find_go", lambda: None)
    with pytest.raises(BridgeError, match="Go toolchain not found"):
        bridge_manager.build_bridge(binary, src)
    assert not os.path.exists(binary)


def test_build_timeout_becomes_bridge_error(build_env, monkeypatch):
    binary, src = build_env

    def timeout(cmd, **kwargs):
        raise bridge_manager.subprocess.TimeoutExpired(cmd, 900)

    monkeypatch.setattr(bridge_manager.subprocess, "run", timeout)
    with pytest.raises(BridgeError, match="timed out after 15 minutes"):
        bridge_manager.build_bridge(binary, src)


def test_build_oserror_becomes_bridge_error(build_env, monkeypatch):
    binary, src = build_env

    def cannot_run(cmd, **kwargs):
        raise PermissionError(errno.EACCES, "Permission denied")

    monkeypatch.setattr(bridge_manager.subprocess, "run", cannot_run)
    with pytest.raises(BridgeError, match="go build could not run: .*Permission denied"):
        bridge_manager.build_bridge(binary, src)


def test_failed_build_removes_partial_output(build_env, monkeypatch):
    binary, src = build_env

    def failing_build(cmd, **kwargs):
        with open(cmd[cmd.index("-o") + 1], "w") as f:  # go leaves a half-written output
            f.write("partial")
        return bridge_manager.subprocess.CompletedProcess(cmd, 2, stdout="syntax error")

    monkeypatch.setattr(bridge_manager.subprocess, "run", failing_build)
    with pytest.raises(BridgeError, match=r"go build failed \(exit 2\):\nsyntax error"):
        bridge_manager.build_bridge(binary, src)
    assert os.listdir(os.path.dirname(binary)) == []


# --- health / api ------------------------------------------------------------------------------


def test_health_with_unparseable_200_means_nothing_answers(monkeypatch):
    monkeypatch.setattr(httpx, "get", lambda url, **kw: httpx.Response(200, content=b"<html>"))
    assert BridgeManager().health() is None


def test_health_rejects_foreign_service_on_the_port(monkeypatch):
    monkeypatch.setattr(httpx, "get", lambda url, **kw: httpx.Response(200, json={"service": "x"}))
    with pytest.raises(BridgeError, match="in use by another service"):
        BridgeManager().health()


def test_health_non_200_means_nothing_answers(monkeypatch):
    monkeypatch.setattr(httpx, "get", lambda url, **kw: httpx.Response(503, json={"a": 1}))
    assert BridgeManager().health() is None


def test_api_transport_error_message(monkeypatch):
    def boom(*args, **kwargs):
        raise httpx.ConnectError("")

    monkeypatch.setattr(httpx, "request", boom)
    with pytest.raises(BridgeError, match="bridge request failed: ConnectError$"):
        BridgeManager().api("GET", "status")

    def boom_msg(*args, **kwargs):
        raise httpx.ReadTimeout("slow")

    monkeypatch.setattr(httpx, "request", boom_msg)
    with pytest.raises(BridgeError, match="bridge request failed: slow$"):
        BridgeManager().api("GET", "status")


# --- revive ------------------------------------------------------------------------------------


def test_revive_gives_up_when_state_changes_during_health_probe(autostart, monkeypatch):
    mgr = BridgeManager()
    mgr.state = "running"

    def racing_probe():
        mgr.state = "starting"  # another caller got there first
        return False

    monkeypatch.setattr(mgr, "_safe_health", racing_probe)
    assert mgr.revive() is False
    assert mgr.state == "starting"


def test_revive_gives_up_when_a_supervisor_thread_is_alive(autostart):
    mgr = BridgeManager()
    mgr.state = "failed"
    mgr._failed_at = -1e9  # long ago: retry is due
    release = threading.Event()
    worker = threading.Thread(target=release.wait, daemon=True)
    worker.start()
    mgr._thread = worker
    try:
        assert mgr.revive() is False
        assert mgr.state == "failed"
    finally:
        release.set()
        worker.join(timeout=5)


# --- ensure_running ----------------------------------------------------------------------------


@pytest.fixture
def run_env(tmp_path, monkeypatch, autostart):
    """Autostart on, private store, no sources by default; health is always scripted."""
    src = tmp_path / "src"
    src.mkdir()
    monkeypatch.setenv("WHATSAPP_STORE_DIR", str(tmp_path / "store"))
    monkeypatch.setenv("WHATSAPP_BRIDGE_SRC", str(src))
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", str(tmp_path / "bridge-bin"))
    return tmp_path, src


def test_stale_sources_without_go_use_existing_binary(run_env, monkeypatch, caplog):
    tmp_path, src = run_env
    binary = tmp_path / "bridge-bin"
    binary.write_text("old")
    os.utime(binary, (1, 1))
    (src / "main.go").write_text("package main\n")  # newer than the binary
    monkeypatch.setattr(bridge_manager, "find_go", lambda: None)
    built = []
    monkeypatch.setattr(bridge_manager, "build_bridge", lambda b, s: built.append(b))
    _script_health(monkeypatch, [None, None])
    spawned = []
    monkeypatch.setattr(BridgeManager, "_spawn", lambda self, b: spawned.append(b) or "proc")
    monkeypatch.setattr(BridgeManager, "_wait_for_health", lambda self, proc, b: None)
    mgr = BridgeManager()
    with caplog.at_level("WARNING", logger="bridge_manager"):
        mgr.ensure_running()
    assert built == [] and spawned == [str(binary)]
    assert "Go is not installed; using it as is" in caplog.text
    assert mgr.state == "running"


def test_missing_binary_and_sources_fails_clearly(run_env, monkeypatch):
    tmp_path, src = run_env
    _script_health(monkeypatch, [None])
    mgr = BridgeManager()
    with pytest.raises(BridgeError, match="not found and no sources in"):
        mgr.ensure_running()
    assert mgr.state == "building"


def test_peer_started_bridge_while_building_is_reused(run_env, monkeypatch):
    tmp_path, src = run_env
    (tmp_path / "bridge-bin").write_text("bin")
    _script_health(monkeypatch, [None, {"service": "whatsapp-bridge"}])
    monkeypatch.setattr(
        BridgeManager, "_spawn", lambda self, b: pytest.fail("must not spawn when a peer answers")
    )
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running"
    assert mgr._done.is_set()


def test_spawn_without_listen_address_leaves_env_untouched(run_env, monkeypatch):
    tmp_path, src = run_env
    monkeypatch.delenv("WHATSAPP_BRIDGE_ADDR", raising=False)
    monkeypatch.setattr(bridge_manager, "bridge_addr", lambda: None)
    seen = {}

    def fake_popen(cmd, **kwargs):
        seen.update(cmd=cmd, env=kwargs["env"])
        return "proc"

    monkeypatch.setattr(bridge_manager.subprocess, "Popen", fake_popen)
    assert BridgeManager()._spawn("/some/bridge") == "proc"
    assert seen["cmd"] == ["/some/bridge"]
    assert "WHATSAPP_BRIDGE_ADDR" not in seen["env"]
    assert seen["env"]["WHATSAPP_STORE_DIR"] == str(tmp_path / "store")


def test_wait_for_health_deadline_reports_log_tail(run_env, monkeypatch):
    tmp_path, src = run_env
    store = tmp_path / "store"
    store.mkdir()
    (store / "bridge.log").write_text("listen failed: boom\n")
    monkeypatch.setenv("WHATSAPP_BRIDGE_START_TIMEOUT", "0")  # deadline already passed
    _script_health(monkeypatch, [None])
    with pytest.raises(BridgeError, match=r"did not answer within 0s:\nlisten failed: boom"):
        BridgeManager()._wait_for_health(None, "/some/bridge")


# --- status ------------------------------------------------------------------------------------


def test_status_manual_mode_with_healthy_bridge(monkeypatch):
    """Autostart off and a bridge answering: reported running with its own status merged in."""
    _script_health(monkeypatch, [{"service": "whatsapp-bridge", "version": "v1"}])
    monkeypatch.setattr(
        BridgeManager,
        "api",
        lambda self, method, path, **kw: {
            "connected": True,
            "logged_in": True,
            "pairing": "idle",
            "pairing_error": "QR expired",
        },
    )
    out = BridgeManager().status()
    assert out == {
        "state": "running",
        "connected": True,
        "logged_in": True,
        "pairing": "idle",
        "pairing_error": "QR expired",
    }


def test_status_running_but_bridge_stopped_answering(monkeypatch):
    mgr = BridgeManager()
    mgr.state = "running"  # autostart is off, so revive() leaves it alone
    _script_health(monkeypatch, [None])
    out = mgr.status()
    assert out["state"] == "failed"
    assert out["error"] == "bridge stopped answering"
    assert out["log_tail"] == ""


def test_safe_health_swallows_bridge_errors(monkeypatch):
    def conflict(self):
        raise BridgeError("another bridge/store")

    monkeypatch.setattr(BridgeManager, "health", conflict)
    assert BridgeManager()._safe_health() is False


def test_api_non_json_body_reports_http_status(monkeypatch):
    monkeypatch.setattr(
        httpx, "request", lambda *a, **kw: httpx.Response(502, content=b"<html>bad gateway</html>")
    )
    with pytest.raises(BridgeError, match=r"bridge returned HTTP 502: <html>bad gateway</html>"):
        BridgeManager().api("GET", "status")
    monkeypatch.setattr(httpx, "request", lambda *a, **kw: httpx.Response(200, json=[1, 2]))
    with pytest.raises(BridgeError, match="bridge returned HTTP 200"):
        BridgeManager().api("GET", "status")


def test_binary_appearing_while_checking_sources_is_used(run_env, monkeypatch):
    """needs_build said yes, but the binary showed up and there is nothing to build from."""
    tmp_path, src = run_env
    binary = tmp_path / "bridge-bin"
    binary.write_text("bin")
    monkeypatch.setattr(bridge_manager, "needs_build", lambda b, s: True)
    monkeypatch.setattr(
        bridge_manager, "build_bridge", lambda b, s: pytest.fail("no sources to build from")
    )
    _script_health(monkeypatch, [None, None])
    spawned = []
    monkeypatch.setattr(BridgeManager, "_spawn", lambda self, b: spawned.append(b) or "proc")
    monkeypatch.setattr(BridgeManager, "_wait_for_health", lambda self, proc, b: None)
    mgr = BridgeManager()
    mgr.ensure_running()
    assert spawned == [str(binary)] and mgr.state == "running"


# --- stale bridge replacement ------------------------------------------------------------------

STALE = {"service": "whatsapp-bridge", "version": "0123456789ab", "pid": 4242}
REAL_PROCESS_NAME = bridge_manager._process_name


def test_is_loopback():
    for host in ("localhost", "127.0.0.1", "127.0.0.2", "::1"):
        assert bridge_manager._is_loopback(host) is True
    for host in ("192.168.0.5", "example.test", "", None):
        assert bridge_manager._is_loopback(host) is False


def test_is_posix_follows_os_name(monkeypatch):
    monkeypatch.setattr(bridge_manager.os, "name", "nt")
    assert bridge_manager._is_posix() is False
    monkeypatch.setattr(bridge_manager.os, "name", "posix")
    assert bridge_manager._is_posix() is True


@pytest.fixture
def replace_env(run_env, monkeypatch):
    """A stale supervisor-built bridge answers; spawning and shutdown requests are recorded."""
    tmp_path, src = run_env
    (src / "main.go").write_text("package main\n")
    current = bridge_manager.source_hash(str(src))
    binary = tmp_path / "bridge-bin"
    binary.write_text("bin")
    (tmp_path / "bridge-bin.srchash").write_text(current)
    monkeypatch.setenv("WHATSAPP_API_BASE_URL", "http://127.0.0.1:9/api")
    monkeypatch.setattr(BridgeManager, "api", lambda self, m, p, **kw: {"pairing": "idle"})
    calls = {"spawned": [], "posts": [], "kills": []}
    monkeypatch.setattr(
        BridgeManager, "_spawn", lambda self, b: calls["spawned"].append(b) or "proc"
    )
    monkeypatch.setattr(BridgeManager, "_wait_for_health", lambda self, proc, b: None)
    monkeypatch.setattr(bridge_manager.os, "kill", lambda pid, sig: calls["kills"].append(pid))
    monkeypatch.setattr(bridge_manager, "_process_name", lambda pid: "bridge-bin")
    monkeypatch.setattr(bridge_manager.time, "sleep", lambda s: None)
    monkeypatch.setattr(bridge_manager, "REPLACE_TIMEOUT", 0.0)
    return calls


def _post_answers(monkeypatch, calls, answer):
    def post(url, **kwargs):
        calls["posts"].append((url, kwargs))
        if isinstance(answer, Exception):
            raise answer
        return httpx.Response(answer, json={})

    monkeypatch.setattr(httpx, "post", post)


def test_request_shutdown_outcomes(replace_env, monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_TOKEN", "tok")
    expected = {200: "ok", 409: "conflict", 404: "unsupported", 405: "unsupported"}
    expected.update({401: "refused", 500: "refused"})
    for code, outcome in expected.items():
        _post_answers(monkeypatch, replace_env, code)
        assert BridgeManager()._request_shutdown("0123456789ab") == outcome
    url, kwargs = replace_env["posts"][0]
    assert url == "http://127.0.0.1:9/api/shutdown" and kwargs["json"] == {
        "version": "0123456789ab"
    }
    assert kwargs["headers"]["Authorization"] == "Bearer tok"
    _post_answers(monkeypatch, replace_env, httpx.ConnectError(""))
    assert BridgeManager()._request_shutdown("0123456789ab") == "error"


def test_source_hash_oserror_means_no_replacement(replace_env, monkeypatch):
    def vanished(src):
        raise FileNotFoundError("main.go")

    monkeypatch.setattr(bridge_manager, "source_hash", vanished)
    monkeypatch.setattr(
        BridgeManager, "api", lambda self, m, p, **kw: pytest.fail("gate must stop earlier")
    )
    _post_answers(monkeypatch, replace_env, 200)
    _script_health(monkeypatch, [STALE])
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running" and replace_env["posts"] == [] and replace_env["spawned"] == []


def test_transport_error_then_still_answering_falls_back_to_sigterm(replace_env, monkeypatch):
    _post_answers(monkeypatch, replace_env, httpx.RemoteProtocolError("dropped"))
    # first run check, wait_gone (timeout 0: still there), pid re-check, then gone
    _script_health(monkeypatch, [STALE, STALE, STALE, None])
    mgr = BridgeManager()
    mgr.ensure_running()
    assert replace_env["kills"] == [4242] and replace_env["spawned"] != []
    assert mgr.state == "running"


def test_conflict_and_bridge_gone_on_recheck_spawns(replace_env, monkeypatch):
    _post_answers(monkeypatch, replace_env, 409)
    _script_health(monkeypatch, [STALE, None])
    mgr = BridgeManager()
    mgr.ensure_running()
    assert replace_env["spawned"] != [] and replace_env["kills"] == []


def test_refused_shutdown_reuses_bridge(replace_env, monkeypatch):
    _post_answers(monkeypatch, replace_env, 401)
    _script_health(monkeypatch, [STALE])
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running" and replace_env["spawned"] == [] and replace_env["kills"] == []


def test_unsupported_shutdown_and_refused_sigterm_reuses_bridge(replace_env, monkeypatch):
    _post_answers(monkeypatch, replace_env, 404)
    _script_health(monkeypatch, [STALE, dict(STALE, pid=999)])  # pid changed before the kill
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running" and replace_env["kills"] == [] and replace_env["spawned"] == []


@pytest.mark.parametrize(
    "now",
    [
        dict(STALE, pid=999),  # another process now answers
        dict(STALE, version="newer"),  # same pid reused by another build
    ],
)
def test_terminate_refuses_when_bridge_changed(replace_env, monkeypatch, now):
    _script_health(monkeypatch, [now])
    assert BridgeManager()._terminate_stale(STALE) is False
    assert replace_env["kills"] == []


@pytest.mark.parametrize("pid", [None, "4242", 0, -1])
def test_terminate_refuses_invalid_pid(replace_env, monkeypatch, pid):
    _script_health(monkeypatch, [dict(STALE, pid=pid)])
    assert BridgeManager()._terminate_stale(dict(STALE, pid=pid)) is False
    assert replace_env["kills"] == []


def test_terminate_refuses_remote_bridge(replace_env, monkeypatch):
    monkeypatch.setenv("WHATSAPP_API_BASE_URL", "http://192.168.0.5:8080/api")
    monkeypatch.setattr(BridgeManager, "health", lambda self: pytest.fail("no health re-read"))
    assert BridgeManager()._terminate_stale(STALE) is False
    assert replace_env["kills"] == []


def test_terminate_refuses_when_port_taken_by_foreign_service(replace_env, monkeypatch):
    def foreign(self):
        raise BridgeError("in use by another service")

    monkeypatch.setattr(BridgeManager, "health", foreign)
    assert BridgeManager()._terminate_stale(STALE) is False
    assert replace_env["kills"] == []


def test_terminate_when_already_gone(replace_env, monkeypatch):
    _script_health(monkeypatch, [None])
    assert BridgeManager()._terminate_stale(STALE) is True
    assert replace_env["kills"] == []


def test_terminate_sends_sigterm(replace_env, monkeypatch):
    sent = []
    monkeypatch.setattr(bridge_manager.os, "kill", lambda pid, sig: sent.append((pid, sig)))
    _script_health(monkeypatch, [STALE])
    assert BridgeManager()._terminate_stale(STALE) is True
    assert sent == [(4242, bridge_manager.signal.SIGTERM)]


def test_terminate_process_lookup_error_means_gone(replace_env, monkeypatch):
    def gone(pid, sig):
        raise ProcessLookupError(pid)

    monkeypatch.setattr(bridge_manager.os, "kill", gone)
    _script_health(monkeypatch, [STALE])
    assert BridgeManager()._terminate_stale(STALE) is True


def test_terminate_permission_error_refuses(replace_env, monkeypatch):
    def denied(pid, sig):
        raise PermissionError(errno.EPERM, "Operation not permitted")

    monkeypatch.setattr(bridge_manager.os, "kill", denied)
    _script_health(monkeypatch, [STALE])
    assert BridgeManager()._terminate_stale(STALE) is False


def test_terminate_refuses_pid_that_runs_another_program(replace_env, monkeypatch):
    """The bridge URL is loopback, but the pid it reports may belong to another namespace."""
    seen = []

    def ps(cmd, **kwargs):
        seen.append(cmd)
        return subprocess.CompletedProcess(cmd, 0, stdout="/usr/bin/python3\n", stderr="")

    monkeypatch.setattr(bridge_manager, "_process_name", REAL_PROCESS_NAME)  # undo the stub
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", "/opt/bridge/whatsapp-bridge")
    monkeypatch.setattr(bridge_manager.subprocess, "run", ps)
    _script_health(monkeypatch, [STALE])
    assert BridgeManager()._terminate_stale(STALE) is False
    assert replace_env["kills"] == [] and seen == [["ps", "-p", "4242", "-o", "comm="]]


@pytest.mark.parametrize(
    ("stdout", "code", "expected"),
    [
        ("/opt/bridge/whatsapp-bridge\n", 0, True),  # macOS: full executable path
        ("whatsapp-bridge\n", 0, True),
        ("whatsapp-bridg\n", 0, False),  # truncation is 15 characters exactly
        ("python3\n", 0, False),
        ("", 1, False),  # no such process
    ],
)
def test_is_bridge_process_matches_binary_basename(monkeypatch, stdout, code, expected):
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", "/opt/bridge/whatsapp-bridge")
    monkeypatch.setattr(
        bridge_manager.subprocess,
        "run",
        lambda cmd, **kw: subprocess.CompletedProcess(cmd, code, stdout=stdout, stderr=""),
    )
    assert bridge_manager._is_bridge_process(4242) is expected


def test_is_bridge_process_accepts_linux_truncated_comm(monkeypatch):
    monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", "/opt/bridge/whatsapp-bridge-dev")
    monkeypatch.setattr(
        bridge_manager.subprocess,
        "run",
        lambda cmd, **kw: subprocess.CompletedProcess(cmd, 0, stdout="whatsapp-bridge\n"),
    )
    assert bridge_manager._is_bridge_process(4242) is True


@pytest.mark.parametrize(
    "error", [OSError(errno.ENOENT, "no ps"), subprocess.TimeoutExpired("ps", 5)]
)
def test_process_name_unreadable_means_not_ours(monkeypatch, error):
    def fail(cmd, **kwargs):
        raise error

    monkeypatch.setattr(bridge_manager.subprocess, "run", fail)
    assert bridge_manager._process_name(4242) is None
    assert bridge_manager._is_bridge_process(4242) is False


def _ps_works() -> bool:
    try:
        return subprocess.run(["ps", "-p", str(os.getpid())], capture_output=True).returncode == 0
    except OSError:
        return False


@pytest.mark.skipif(not _ps_works(), reason="ps is not available (sandbox)")
def test_is_bridge_process_with_real_ps(monkeypatch):
    """Real ps against a real child: the binary basename decides."""
    child = subprocess.Popen(["sleep", "30"])
    try:
        monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", "/somewhere/sleep")
        assert bridge_manager._is_bridge_process(child.pid) is True
        monkeypatch.setenv("WHATSAPP_BRIDGE_BIN", "/somewhere/whatsapp-bridge")
        assert bridge_manager._is_bridge_process(child.pid) is False
    finally:
        child.kill()
        child.wait()


@pytest.mark.parametrize(
    ("release", "expected"),
    [
        ("0.3.0", (0, 3, 0)),
        ("v1.2.0-rc1", (1, 2, 0)),
        ("10", (10,)),
        (None, ()),  # pre-0.3 bridges report no release
        ("", ()),
        ("dev", ()),
        (3, ()),
    ],
)
def test_release_tuple(release, expected):
    assert bridge_manager._release_tuple(release) == expected


def test_bridge_is_newer_compares_releases_numerically(monkeypatch):
    monkeypatch.setattr(bridge_manager, "RELEASE", "0.10.0")
    assert bridge_manager._bridge_is_newer({"release": "0.10.1"}) is True
    assert bridge_manager._bridge_is_newer({"release": "0.9.9"}) is False
    assert bridge_manager._bridge_is_newer({"release": "0.10.0"}) is False
    assert bridge_manager._bridge_is_newer({}) is False


def test_oserror_during_forced_build_reuses_the_old_bridge(replace_env, monkeypatch, tmp_path):
    """source_hash or makedirs failing inside build_bridge must not fail the supervisor."""
    (tmp_path / "bridge-bin.srchash").write_text("older-sidecar")
    monkeypatch.setattr(bridge_manager, "find_go", lambda: "/fake/go")

    def broken_build(binary, src):
        raise PermissionError(errno.EACCES, "Permission denied", binary)

    monkeypatch.setattr(bridge_manager, "build_bridge", broken_build)
    _post_answers(monkeypatch, replace_env, 200)
    _script_health(monkeypatch, [STALE])
    mgr = BridgeManager()
    mgr.ensure_running()
    assert mgr.state == "running" and mgr.error is None and mgr._done.is_set()
    assert replace_env["posts"] == [] and replace_env["spawned"] == []
