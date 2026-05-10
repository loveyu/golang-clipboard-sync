#!/usr/bin/env python3
"""Mock servers for clipboard-sync integration testing.

Center server (port 9999):
  PUT   /client/{client}/{msgId}  - store data (Bearer token auth)
  GET   /client/{client}/{msgId}  - retrieve data (Bearer token auth)
  GET   /_stats                   - list all stored data for verification

HTTP capture server (port 9998):
  POST  /update-clipboard         - capture clipboard sync messages
  GET   /_stats                   - list all captured messages for verification
"""

import json
import sys
import threading
from http.server import HTTPServer, BaseHTTPRequestHandler

# Shared state
lock = threading.Lock()
center_data = {}     # {client/msgId: (bytes, content_type)}
center_put_log = []  # [{key, size, content_type}]
http_messages = []   # [{path, body, size}]

EXPECTED_TOKEN = "test-integration-token"


class CenterHandler(BaseHTTPRequestHandler):
    """Mock clipboard center server."""

    def do_PUT(self):
        if not self._check_auth():
            return
        parts = self.path.strip("/").split("/")
        if len(parts) == 3 and parts[0] == "client":
            length = int(self.headers.get("Content-Length", 0))
            data = self.rfile.read(length)
            ct = self.headers.get("Content-Type", "application/octet-stream")
            key = f"{parts[1]}/{parts[2]}"
            with lock:
                center_data[key] = (data, ct)
                center_put_log.append({"key": key, "size": len(data), "content_type": ct})
            self.send_response(204)
            self.end_headers()
        else:
            self.send_response(404)
            self.end_headers()

    def do_GET(self):
        if self.path == "/_stats":
            with lock:
                body = json.dumps({"puts": center_put_log, "keys": list(center_data.keys())})
            self._send_json(200, body)
            return
        if not self._check_auth():
            return
        parts = self.path.strip("/").split("/")
        if len(parts) == 3 and parts[0] == "client":
            key = f"{parts[1]}/{parts[2]}"
            with lock:
                entry = center_data.get(key)
            if entry:
                data, ct = entry
                self.send_response(200)
                if ct:
                    self.send_header("Content-Type", ct)
                self.end_headers()
                self.wfile.write(data)
            else:
                self.send_response(404)
                self.end_headers()
        else:
            self.send_response(404)
            self.end_headers()

    def _check_auth(self):
        auth = self.headers.get("Authorization", "")
        if auth != f"Bearer {EXPECTED_TOKEN}":
            self.send_response(401)
            self.end_headers()
            return False
        return True

    def _send_json(self, code, body):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *_):
        pass


class CaptureHandler(BaseHTTPRequestHandler):
    """HTTP target capture server."""

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        data = self.rfile.read(length)
        with lock:
            http_messages.append({"path": self.path, "body": data.decode(), "size": len(data)})
        self._send_json(200, '{"status":"ok"}')

    def do_GET(self):
        if self.path == "/_stats":
            with lock:
                body = json.dumps({"messages": http_messages, "count": len(http_messages)})
            self._send_json(200, body)
        else:
            self.send_response(404)
            self.end_headers()

    def _send_json(self, code, body):
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body.encode())

    def log_message(self, *_):
        pass


if __name__ == "__main__":
    center_port = int(sys.argv[1]) if len(sys.argv) > 1 else 9999
    capture_port = int(sys.argv[2]) if len(sys.argv) > 2 else 9998

    center_srv = HTTPServer(("0.0.0.0", center_port), CenterHandler)
    capture_srv = HTTPServer(("0.0.0.0", capture_port), CaptureHandler)

    threading.Thread(target=center_srv.serve_forever, daemon=True).start()
    threading.Thread(target=capture_srv.serve_forever, daemon=True).start()

    print(f"READY center=:{center_port} capture=:{capture_port}", flush=True)

    try:
        threading.Event().wait()
    except KeyboardInterrupt:
        pass
