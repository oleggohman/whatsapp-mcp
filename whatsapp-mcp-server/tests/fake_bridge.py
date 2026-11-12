"""Fake whatsapp-bridge used as WHATSAPP_BRIDGE_BIN in supervisor tests (stdlib only).

Behaviour is driven by environment variables:
  FAKE_EXIT_EARLY=<code>  print "boom" to stderr and exit immediately
  FAKE_START_DELAY=<s>    sleep after taking the lock, before listening
  FAKE_VERSION            version reported by /api/health (default "fakev")
  FAKE_TOKEN              token required instead of <store>/bridge.token
  FAKE_LOGGED_IN=1        report an already paired account
  FAKE_REPLACED=1         start in pairing state "replaced" (another client took the login)
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
state = {
    "pairing": "replaced" if os.environ.get("FAKE_REPLACED") == "1" else "idle",
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


time.sleep(float(os.environ.get("FAKE_START_DELAY", "0")))


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

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
        url = urlsplit(self.path)
        if url.path == "/api/health":
            return self._send(
                200,
                {
                    "service": "whatsapp-bridge",
                    "version": os.environ.get("FAKE_VERSION", "fakev"),
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
        if self.path == "/api/send":
            if not state["logged_in"]:
                return self._send(200, {"success": False, "message": "Not connected to WhatsApp"})
            return self._send(200, {"success": True, "message": "sent"})
        self._send(404, {"message": "not found"})


host, _, port = addr.rpartition(":")
server = ThreadingHTTPServer((host.strip("[]") or "127.0.0.1", int(port)), Handler)
threading.Timer(120, lambda: os._exit(0)).start()  # safety net: never outlive a test run
server.serve_forever()
