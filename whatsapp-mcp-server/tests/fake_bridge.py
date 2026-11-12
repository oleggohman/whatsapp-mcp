"""Fake whatsapp-bridge used as WHATSAPP_BRIDGE_BIN in supervisor tests (stdlib only).

Behaviour is driven by environment variables:
  FAKE_EXIT_EARLY=<code>  print "boom" to stderr and exit immediately
  FAKE_START_DELAY=<s>    sleep after taking the lock, before listening
  FAKE_EXIT_AFTER=<s>     hold the lock without listening for s seconds, then exit (a peer
                          that is shutting down: listener closed, lock not yet released)
  FAKE_VERSION            version reported by /api/health (default "fakev")
  FAKE_TOKEN              token required instead of <store>/bridge.token
  FAKE_LOGGED_IN=1        report an already paired account
  FAKE_REPLACED=1         start in pairing state "replaced" (another client took the login)
  FAKE_PAIRING=<state>    start in this pairing state (e.g. "qr" or "code": pairing in progress)
  FAKE_SHUTDOWN=<mode>    POST /api/shutdown behaviour: unset = graceful exit like the real
                          bridge; "missing" = 404 (a v0.2.0 bridge without the endpoint);
                          "ignore" = answer 200 but keep running; "drop" = exit without answering
  FAKE_SHUTDOWN_VERSION   version /api/shutdown must be called with (default FAKE_VERSION); 409
                          otherwise
  FAKE_SHUTDOWN_HOLD=<s>  after a shutdown, hold the lock s seconds with the listener closed
Every request is appended as "METHOD /path" to <store>/requests.log.
The lock, store dir, address and token file follow the real bridge's rules.
"""

import fcntl
import json
import os
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlsplit

store = os.path.abspath(os.environ["WHATSAPP_STORE_DIR"])
addr = os.environ.get("WHATSAPP_BRIDGE_ADDR", "127.0.0.1:8080")

if os.environ.get("FAKE_EXIT_EARLY"):
    print("boom: fake bridge failing on purpose", file=sys.stderr, flush=True)
    sys.exit(int(os.environ["FAKE_EXIT_EARLY"]))

os.makedirs(store, exist_ok=True)
lock = open(os.path.join(store, "bridge.lock"), "w")
try:
    fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
except OSError:
    print("lock held by a peer", file=sys.stderr, flush=True)
    sys.exit(75)

with open(os.path.join(store, "spawns.log"), "a") as f:
    f.write(f"{os.getpid()}\n")
with open(os.path.join(store, "fake.pid"), "w") as f:
    f.write(str(os.getpid()))

token = os.environ.get("FAKE_TOKEN")
if not token:
    try:
        token = open(os.path.join(store, "bridge.token")).read().strip()
    except OSError:
        token = ""
version = os.environ.get("FAKE_VERSION", "fakev")
state = {
    "pairing": os.environ.get("FAKE_PAIRING")
    or ("replaced" if os.environ.get("FAKE_REPLACED") == "1" else "idle"),
    "logged_in": os.environ.get("FAKE_LOGGED_IN") == "1",
}


def status_body():
    body = {
        "connected": state["logged_in"] and state["pairing"] != "replaced",
        "logged_in": state["logged_in"],
        "pairing": state["pairing"],
    }
    if state["pairing"] == "qr":
        body["qr"] = "2@fakeqrpayload,abc,def"
    return body


if os.environ.get("FAKE_EXIT_AFTER"):
    time.sleep(float(os.environ["FAKE_EXIT_AFTER"]))
    sys.exit(0)

time.sleep(float(os.environ.get("FAKE_START_DELAY", "0")))


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _record(self):
        with open(os.path.join(store, "requests.log"), "a") as f:
            f.write(f"{self.command} {urlsplit(self.path).path}\n")

    def _send(self, code, body):
        data = json.dumps(body).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def _authed(self):
        if token and self.headers.get("Authorization") != f"Bearer {token}":
            self._send(401, {"message": "unauthorized"})
            return False
        return True

    def do_GET(self):
        self._record()
        url = urlsplit(self.path)
        if url.path == "/api/health":
            return self._send(
                200,
                {
                    "service": "whatsapp-bridge",
                    "version": version,
                    "store_dir": store,
                    "pid": os.getpid(),
                    "auth_required": bool(token),
                },
            )
        if url.path == "/api/status":
            if not self._authed():
                return
            query = parse_qs(url.query)
            if "start_pairing" in query or "reconnect" in query:
                return self._send(400, {"success": False, "message": "status is read-only"})
            return self._send(200, status_body())
        self._send(404, {"message": "not found"})

    def do_POST(self):
        self._record()
        if not self._authed():
            return
        length = int(self.headers.get("Content-Length") or 0)
        payload = json.loads(self.rfile.read(length) or b"{}")
        if self.path == "/api/pair-phone":
            if state["logged_in"]:
                return self._send(409, {"success": False, "message": "already logged in"})
            if not str(payload.get("phone", "")).isdigit():
                return self._send(400, {"success": False, "message": "invalid phone number"})
            state["pairing"] = "code"
            return self._send(200, {"success": True, "code": "ABCD-EFGH"})
        if self.headers.get("Content-Type") != "application/json":
            return self._send(415, {"message": "Content-Type must be application/json"})
        if self.path == "/api/start-pairing":
            if not state["logged_in"]:
                state["pairing"] = "qr"
            return self._send(200, status_body())
        if self.path == "/api/reconnect":
            if not state["logged_in"]:
                return self._send(409, {"success": False, "message": "not logged in"})
            state["pairing"] = "idle"
            return self._send(200, status_body())
        if self.path == "/api/shutdown":
            return self._shutdown(payload)
        if self.path == "/api/send":
            if not state["logged_in"]:
                return self._send(200, {"success": False, "message": "Not connected to WhatsApp"})
            return self._send(200, {"success": True, "message": "sent"})
        self._send(404, {"message": "not found"})

    def _shutdown(self, payload):
        mode = os.environ.get("FAKE_SHUTDOWN", "")
        if mode == "missing":
            return self._send(404, {"message": "not found"})
        if mode == "drop":
            os._exit(0)
        if payload.get("version") != os.environ.get("FAKE_SHUTDOWN_VERSION", version):
            return self._send(409, {"message": "Version mismatch"})
        self._send(200, {"success": True, "message": "Shutting down"})
        if mode != "ignore":
            threading.Thread(target=stop).start()


def stop():
    """Graceful exit: close the listener, keep the lock FAKE_SHUTDOWN_HOLD seconds, exit 0."""
    server.shutdown()
    server.server_close()
    time.sleep(float(os.environ.get("FAKE_SHUTDOWN_HOLD", "0")))
    os._exit(0)


host, _, port = addr.rpartition(":")
server = ThreadingHTTPServer((host.strip("[]") or "127.0.0.1", int(port)), Handler)
threading.Timer(120, lambda: os._exit(0)).start()  # safety net: never outlive a test run
server.serve_forever()
