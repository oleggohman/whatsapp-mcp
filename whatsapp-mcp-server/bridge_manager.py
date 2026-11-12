"""Supervisor for the Go bridge: build, spawn and health-check a shared per-store daemon.

Lifecycle model: the bridge is a daemon, one per store dir. Any MCP client spawns it when it is
absent and none of them ever stops it (it exits via WHATSAPP_BRIDGE_IDLE_EXIT, kill or reboot).
When it is gone, the next bridge-dependent call starts it again (see BridgeManager.revive).
stdout carries the MCP stdio transport, so everything here logs to stderr via logging.
"""

import glob
import hashlib
import logging
import os
import secrets
import shutil
import subprocess
import threading
import time
import uuid
from typing import Any
from urllib.parse import urlsplit

import httpx

try:
    import fcntl
except ImportError:  # Windows: recovery runs without the advisory lock
    fcntl = None  # type: ignore[assignment]

logger = logging.getLogger(__name__)

SERVER_DIR = os.path.dirname(os.path.abspath(__file__))
DEFAULT_STORE_DIR = os.path.join(SERVER_DIR, "..", "whatsapp-bridge", "store")
DEFAULT_SRC_DIR = os.path.join(SERVER_DIR, "..", "whatsapp-bridge")
DEFAULT_BIN = os.path.join(DEFAULT_SRC_DIR, "whatsapp-bridge")
DEFAULT_API_BASE_URL = "http://localhost:8080/api"

EX_TEMPFAIL = 75  # bridge exit code: a peer bridge holds the store lock (running or starting)
LOG_ROTATE_BYTES = 5 * 1024 * 1024
LOG_TAIL_BYTES = 4096
FAILED_RETRY_SECONDS = 30.0  # a failed start is retried by the next bridge call after this
HEALTH_TIMEOUT = httpx.Timeout(3.0, connect=1.0)
API_TIMEOUT = httpx.Timeout(30.0, connect=3.0)

GO_PROBE_PATHS = (
    "/opt/homebrew/bin/go",
    "/usr/local/go/bin/go",
    "/usr/local/bin/go",
    "~/go/bin/go",
    "~/sdk/*/bin/go",
)


class BridgeError(RuntimeError):
    """A bridge supervision failure with a user-facing message."""


# --- configuration (read at call time so tests and clients can change the env) ---------------


def store_dir() -> str:
    """Absolute store dir shared by the bridge and this server (WHATSAPP_STORE_DIR)."""
    return os.path.abspath(os.environ.get("WHATSAPP_STORE_DIR") or DEFAULT_STORE_DIR)


def api_base_url() -> str:
    return (os.environ.get("WHATSAPP_API_BASE_URL") or DEFAULT_API_BASE_URL).rstrip("/")


def bridge_addr() -> str | None:
    """Listen address handed to the child: WHATSAPP_BRIDGE_ADDR, else derived from the API URL."""
    addr = os.environ.get("WHATSAPP_BRIDGE_ADDR")
    if addr:
        return addr
    parts = urlsplit(api_base_url())
    host = parts.hostname or "127.0.0.1"
    if host == "localhost":
        host = "127.0.0.1"
    return f"{host}:{parts.port or 80}"


def autostart_enabled() -> bool:
    return os.environ.get("WHATSAPP_BRIDGE_AUTOSTART", "1").strip().lower() not in (
        "0",
        "false",
        "no",
        "off",
    )


def bridge_bin() -> str:
    return os.path.abspath(os.environ.get("WHATSAPP_BRIDGE_BIN") or DEFAULT_BIN)


def src_dir() -> str:
    return os.path.abspath(os.environ.get("WHATSAPP_BRIDGE_SRC") or DEFAULT_SRC_DIR)


def start_timeout() -> float:
    try:
        return float(os.environ.get("WHATSAPP_BRIDGE_START_TIMEOUT", "60"))
    except ValueError:
        return 60.0


# --- token ---------------------------------------------------------------------------------


def token_path() -> str:
    return os.path.join(store_dir(), "bridge.token")


def read_token() -> str | None:
    """Token from WHATSAPP_BRIDGE_TOKEN, else <store>/bridge.token; None when neither exists."""
    env = (os.environ.get("WHATSAPP_BRIDGE_TOKEN") or "").strip()
    if env:
        return env
    try:
        with open(token_path(), encoding="utf-8") as f:
            return f.read().strip() or None
    except OSError:
        return None


def _create_token_file(tmp: str, path: str, token: str) -> bool:
    """Atomically create path holding token; False when path already exists.

    Hard-links the fully written temp file into place. Filesystems without hard links
    (exFAT, some network mounts) fall back to O_CREAT|O_EXCL plus write and fsync.
    """
    try:
        os.link(tmp, path)
        return True
    except FileExistsError:
        return False
    except OSError:
        pass
    try:
        fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    except FileExistsError:
        return False
    with open(fd, "w", encoding="utf-8") as f:
        f.write(token)
        f.flush()
        os.fsync(f.fileno())
    return True


def _recover_empty_token(tmp: str, path: str, token: str) -> str:
    """Replace a stale empty token file; concurrent clients end up with one token.

    The recovery runs under an exclusive advisory lock on the store directory and re-reads
    the file under it, so only the first client replaces the file and the rest read its token.
    """
    lock_fd = None
    if fcntl is not None:
        lock_fd = os.open(os.path.dirname(path), os.O_RDONLY)
        fcntl.flock(lock_fd, fcntl.LOCK_EX)
    try:
        existing = read_token()
        if existing:
            return existing
        logger.warning("token file %s is empty (left by a crash); replacing it", path)
        os.replace(tmp, path)
        return token
    finally:
        if lock_fd is not None:
            os.close(lock_fd)


def ensure_token() -> str:
    """Return the token, creating <store>/bridge.token (0600) if needed.

    The token is written to a private temp file first and then hard-linked into place, which
    is atomic and fails if the file exists: the token file is never visible empty, even after
    a crash. An empty file (left by an older version that created first and wrote second) is
    treated as stale and replaced.
    """
    existing = read_token()
    if existing:
        return existing
    path = token_path()
    os.makedirs(os.path.dirname(path), exist_ok=True)
    token = secrets.token_hex(32)
    tmp = f"{path}.tmp.{os.getpid()}.{uuid.uuid4().hex[:8]}"
    try:
        fd = os.open(tmp, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with open(fd, "w", encoding="utf-8") as f:
            f.write(token)
            f.flush()
            os.fsync(f.fileno())
        if _create_token_file(tmp, path, token):
            return token
        # A peer created it first; an older peer may still be writing, so retry briefly.
        for _ in range(50):
            existing = read_token()
            if existing:
                return existing
            time.sleep(0.05)
        return _recover_empty_token(tmp, path, token)
    except OSError as e:
        raise BridgeError(f"could not create token file {path}: {e}") from e
    finally:
        if os.path.exists(tmp):
            os.unlink(tmp)


# --- build ---------------------------------------------------------------------------------


def find_go() -> str | None:
    """Locate the go tool: WHATSAPP_GO_BIN, PATH, then common install locations."""
    explicit = os.environ.get("WHATSAPP_GO_BIN")
    if explicit:
        return shutil.which(explicit) or (explicit if os.path.isfile(explicit) else None)
    found = shutil.which("go")
    if found:
        return found
    for pattern in GO_PROBE_PATHS:
        for candidate in sorted(glob.glob(os.path.expanduser(pattern))):
            if os.path.isfile(candidate) and os.access(candidate, os.X_OK):
                return candidate
    return None


def _source_files(src: str) -> list[str]:
    files = [f for f in glob.glob(os.path.join(src, "*.go")) if not f.endswith("_test.go")]
    files.append(os.path.join(src, "go.sum"))
    return sorted(f for f in files if os.path.isfile(f))


def source_hash(src: str) -> str | None:
    """Short hash of the bridge sources (*.go without tests, go.sum); None without sources."""
    files = _source_files(src)
    if not files:
        return None
    h = hashlib.sha256()
    for f in files:
        h.update(os.path.basename(f).encode())
        with open(f, "rb") as fh:
            h.update(fh.read())
    return h.hexdigest()[:12]


def _hash_sidecar(binary: str) -> str:
    return binary + ".srchash"


def built_version(binary: str | None = None) -> str | None:
    """Source hash stamped into the binary this supervisor built (None if it did not build it)."""
    try:
        with open(_hash_sidecar(binary or bridge_bin()), encoding="utf-8") as f:
            return f.read().strip() or None
    except OSError:
        return None


def needs_build(binary: str, src: str) -> bool:
    if not os.path.isfile(binary):
        return True
    files = _source_files(src)
    if not files:
        return False
    return max(os.path.getmtime(f) for f in files) > os.path.getmtime(binary)


def _tail(text: str, limit: int = LOG_TAIL_BYTES) -> str:
    return text[-limit:].strip()


_build_lock = threading.Lock()


def build_bridge(binary: str, src: str) -> None:
    """go build into a temp file, then atomically replace the binary. Output is captured."""
    go = find_go()
    if go is None:
        raise BridgeError(
            "Go toolchain not found. Install Go (https://go.dev/dl/), or set WHATSAPP_GO_BIN, "
            "or point WHATSAPP_BRIDGE_BIN at a prebuilt bridge binary."
        )
    version = source_hash(src) or "unknown"
    env = dict(os.environ, CGO_ENABLED="1")
    os.makedirs(os.path.dirname(binary), exist_ok=True)
    tmp = f"{binary}.tmp.{os.getpid()}.{uuid.uuid4().hex[:8]}"
    tmp_hash = _hash_sidecar(tmp)
    cmd = [go, "build", "-ldflags", f"-X main.bridgeVersion={version}", "-o", tmp, "."]
    logger.info("building bridge: %s (cwd %s)", " ".join(cmd), src)
    try:
        with _build_lock:
            proc = subprocess.run(
                cmd,
                cwd=src,
                env=env,
                stdin=subprocess.DEVNULL,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                errors="replace",
                timeout=900,
                check=False,
            )
        if proc.returncode != 0:
            raise BridgeError(
                f"go build failed (exit {proc.returncode}):\n{_tail(proc.stdout or '')}"
            )
        os.replace(tmp, binary)
        # Replace the sidecar atomically too: status() may read it concurrently.
        with open(tmp_hash, "w", encoding="utf-8") as f:
            f.write(version)
        os.replace(tmp_hash, _hash_sidecar(binary))
    except subprocess.TimeoutExpired as e:
        raise BridgeError("go build timed out after 15 minutes") from e
    except OSError as e:
        raise BridgeError(f"go build could not run: {e}") from e
    finally:
        for leftover in (tmp, tmp_hash):
            if os.path.exists(leftover):
                os.unlink(leftover)


# --- manager -------------------------------------------------------------------------------


def _same_dir(a: str, b: str) -> bool:
    return os.path.realpath(a) == os.path.realpath(b)


class BridgeManager:
    """Builds, starts and watches the bridge daemon. Thread-safe; blocking calls run in threads."""

    def __init__(self) -> None:
        self.state = "idle"  # idle | building | starting | running | failed
        self.error: str | None = None
        self._lock = threading.Lock()
        self._thread: threading.Thread | None = None
        self._done = threading.Event()  # set once state is running or failed
        self._failed_at = 0.0  # time.monotonic() of the last failure

    # -- http helpers --

    def _headers(self) -> dict[str, str]:
        headers = {"Content-Type": "application/json"}
        token = read_token()
        if token:
            headers["Authorization"] = f"Bearer {token}"
        return headers

    def health(self) -> dict[str, Any] | None:
        """GET /api/health; None when nothing answers. A non-bridge answer raises BridgeError."""
        try:
            r = httpx.get(f"{api_base_url()}/health", timeout=HEALTH_TIMEOUT)
        except httpx.HTTPError:
            return None
        try:
            body = r.json() if r.status_code == 200 else None
        except ValueError:
            body = None
        if not isinstance(body, dict) or body.get("service") != "whatsapp-bridge":
            if r.status_code == 200 and body is not None:
                raise BridgeError(
                    f"{api_base_url()} is in use by another service, not the WhatsApp bridge"
                )
            return None
        if not _same_dir(str(body.get("store_dir", "")), store_dir()):
            raise BridgeError(
                f"port in use by another bridge/store: it serves {body.get('store_dir')!r}, "
                f"this server uses {store_dir()!r}. "
                "Set WHATSAPP_STORE_DIR / WHATSAPP_BRIDGE_ADDR consistently."
            )
        return body

    def api(self, method: str, path: str, **kwargs: Any) -> dict[str, Any]:
        """Authenticated bridge call; returns the JSON body. Raises BridgeError on failure."""
        try:
            r = httpx.request(
                method,
                f"{api_base_url()}/{path.lstrip('/')}",
                headers=self._headers(),
                timeout=API_TIMEOUT,
                **kwargs,
            )
        except httpx.HTTPError as e:
            raise BridgeError(f"bridge request failed: {str(e) or type(e).__name__}") from e
        if r.status_code == 401:
            raise BridgeError(
                "bridge running with unknown token: set WHATSAPP_BRIDGE_TOKEN to the token it "
                f"was started with, or stop it (kill the pid from {api_base_url()}/health)"
            )
        try:
            body = r.json()
        except ValueError:
            body = None
        if not isinstance(body, dict):
            raise BridgeError(f"bridge returned HTTP {r.status_code}: {_tail(r.text, 300)}")
        if r.status_code >= 400:
            raise BridgeError(str(body.get("message") or f"bridge returned HTTP {r.status_code}"))
        return body

    # -- lifecycle --

    def start_background(self) -> None:
        """Start ensure_running in a daemon thread (idempotent). Returns immediately."""
        with self._lock:
            if self._thread is not None:
                return
            self._thread = threading.Thread(target=self._run, name="bridge-supervisor", daemon=True)
            self._thread.start()

    def _run(self) -> None:
        try:
            self.ensure_running()
        except Exception as e:  # noqa: BLE001 - surfaced through status, never raised into the loop
            self._fail(str(e) or type(e).__name__)

    def _fail(self, message: str) -> None:
        logger.error("bridge supervisor: %s", message)
        self.state, self.error = "failed", message
        self._failed_at = time.monotonic()
        self._done.set()

    def revive(self) -> bool:
        """Start a new supervision run when the daemon died after it was running (idle exit,
        crash, kill) or when the last failure is older than FAILED_RETRY_SECONDS.

        Returns True when a new run was started.
        """
        if not autostart_enabled():
            return False
        state = self.state
        if state == "running":
            if self._safe_health():
                return False
            logger.warning("bridge stopped answering; starting it again")
        elif state == "failed":
            if time.monotonic() - self._failed_at < FAILED_RETRY_SECONDS:
                return False
            logger.info("retrying bridge start after failure: %s", self.error)
        else:
            return False
        with self._lock:
            # Another caller may have revived it meanwhile.
            if self.state != state or (self._thread is not None and self._thread.is_alive()):
                return False
            self.state = "starting"
            self._done.clear()
            self._thread = threading.Thread(target=self._run, name="bridge-supervisor", daemon=True)
            self._thread.start()
        return True

    def wait_ready(self, timeout: float) -> str:
        """Block up to timeout for the supervisor; returns the state (running|failed|...)."""
        if not autostart_enabled():
            return "running"  # manual mode: the user manages the bridge
        if not self.revive():
            self.start_background()
        self._done.wait(timeout)
        return self.state

    def ensure_running(self) -> None:
        """Make sure a bridge for our store dir answers on the configured address."""
        if not autostart_enabled():
            logger.info("bridge autostart disabled (WHATSAPP_BRIDGE_AUTOSTART=0)")
            return
        self.error = None
        ensure_token()  # before any spawn so every client shares the same token file

        if self.health() is not None:
            logger.info("reusing bridge already running on %s", api_base_url())
            self.state = "running"
            self._done.set()
            return

        binary, src = bridge_bin(), src_dir()
        if needs_build(binary, src):
            self.state = "building"
            if os.path.isdir(src) and _source_files(src):
                if find_go() is None and os.path.isfile(binary):
                    logger.warning(
                        "sources are newer than %s but Go is not installed; using it as is", binary
                    )
                else:
                    build_bridge(binary, src)
            elif not os.path.isfile(binary):
                raise BridgeError(f"bridge binary {binary} not found and no sources in {src}")

        self.state = "starting"
        # Re-check: a peer may have started it while we were building.
        if self.health() is not None:
            self.state = "running"
            self._done.set()
            return
        proc = self._spawn(binary)
        self._wait_for_health(proc)
        self.state = "running"
        self._done.set()

    def _log_path(self) -> str:
        return os.path.join(store_dir(), "bridge.log")

    def _spawn(self, binary: str) -> "subprocess.Popen[bytes]":
        os.makedirs(store_dir(), exist_ok=True)
        log = self._log_path()
        try:
            if os.path.getsize(log) > LOG_ROTATE_BYTES:
                os.replace(log, log + ".1")
        except OSError:
            pass
        env = dict(os.environ, WHATSAPP_STORE_DIR=store_dir())
        addr = bridge_addr()
        if addr:
            env["WHATSAPP_BRIDGE_ADDR"] = addr
        logger.info("starting bridge %s (store %s, addr %s)", binary, store_dir(), addr)
        with open(log, "ab") as logf:
            return subprocess.Popen(  # noqa: S603 - our own binary
                [binary],
                cwd=store_dir(),
                env=env,
                stdin=subprocess.DEVNULL,
                stdout=logf,
                stderr=logf,
                start_new_session=True,
            )

    def _wait_for_health(self, proc: "subprocess.Popen[bytes] | None") -> None:
        deadline = time.monotonic() + start_timeout()
        while time.monotonic() < deadline:
            if self.health() is not None:
                return
            if proc is not None:
                code = proc.poll()
                if code == EX_TEMPFAIL:
                    logger.info("bridge peer holds the store lock; waiting for it to answer")
                    proc = None  # a peer won the race: keep polling health
                elif code is not None:
                    raise BridgeError(f"bridge exited with code {code}:\n{self.log_tail()}")
            time.sleep(0.2)
        raise BridgeError(
            f"bridge did not answer within {start_timeout():.0f}s:\n{self.log_tail()}"
        )

    def log_tail(self) -> str:
        try:
            with open(self._log_path(), "rb") as f:
                f.seek(0, os.SEEK_END)
                f.seek(max(0, f.tell() - LOG_TAIL_BYTES))
                return f.read().decode("utf-8", "replace").strip()
        except OSError:
            return ""

    # -- status for tools --

    def status(self) -> dict[str, Any]:
        """Snapshot for get_connection_status: supervisor state plus the bridge's own status."""
        self.revive()
        out: dict[str, Any] = {"state": self.state}
        if self.state == "idle" and not autostart_enabled():
            out["state"] = "running" if self._safe_health() else "failed"
            if out["state"] == "failed":
                out["error"] = (
                    "autostart is disabled (WHATSAPP_BRIDGE_AUTOSTART=0) and no bridge answers"
                )
        if self.error:
            out["error"] = self.error
        if out["state"] == "failed":
            out["log_tail"] = self.log_tail()
            return out
        if out["state"] != "running":
            return out
        try:
            health = self.health()
            if health is None:
                out.update(
                    state="failed", error="bridge stopped answering", log_tail=self.log_tail()
                )
                return out
            st = self.api("GET", "status")
        except BridgeError as e:
            out["error"] = str(e)
            return out
        out.update({k: st.get(k) for k in ("connected", "logged_in", "pairing")})
        if st.get("pairing_error"):
            out["pairing_error"] = st["pairing_error"]
        expected = built_version()
        if expected and health.get("version") != expected:
            out["restart_needed"] = True
            out["hint"] = (
                f"The running bridge (pid {health.get('pid')}) was built from older sources. "
                f"Stop it (kill {health.get('pid')}); the next bridge call spawns the new build."
            )
            logger.warning(
                "bridge daemon is stale: running %s, built %s", health.get("version"), expected
            )
        return out

    def _safe_health(self) -> bool:
        try:
            return self.health() is not None
        except BridgeError:
            return False


_manager: BridgeManager | None = None
_manager_lock = threading.Lock()


def get_manager() -> BridgeManager:
    global _manager
    with _manager_lock:
        if _manager is None:
            _manager = BridgeManager()
        return _manager


def reset_manager() -> None:
    """Drop the singleton (tests)."""
    global _manager
    with _manager_lock:
        _manager = None
