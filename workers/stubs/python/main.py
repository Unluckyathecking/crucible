"""
Hello-world Crucible worker stub (Python, stdlib-only).

Implements the frozen HTTP/JSON contract:
  POST /invoke  — echoes the request payload; reads billable_units from metadata["units"]
  GET  /healthz — returns {"status": "ok"}

Invariant: billable_units >= 1 on every successful /invoke response.

Run:  python3 main.py [PORT]   (default port 8081)

Smoke test:
  curl -X POST localhost:8081/invoke \
    -H 'content-type: application/json' \
    -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'
"""

import json
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def _parse_units(metadata: dict) -> int:
    try:
        n = int(metadata.get("units", "1"))
        return n if n >= 1 else 1
    except (ValueError, TypeError):
        return 1


class WorkerHandler(BaseHTTPRequestHandler):
    def log_message(self, fmt, *args):  # suppress default access log noise
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self._send_json({"status": "ok"})
        else:
            self.send_error(404)

    def do_POST(self):
        if self.path != "/invoke":
            self.send_error(404)
            return

        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length)
        try:
            req = json.loads(body)
        except json.JSONDecodeError:
            self._send_json({"error": {"code": "BAD_REQUEST", "message": "invalid JSON"}})
            return

        metadata = req.get("metadata") or {}
        units = _parse_units(metadata)

        resp = {
            "payload": {"echo": req.get("payload"), "operation": req.get("operation")},
            "billable_units": units,
        }
        self._send_json(resp)

    def _send_json(self, obj: dict):
        data = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


def main():
    port = 8081
    if len(sys.argv) > 1:
        try:
            port = int(sys.argv[1])
        except ValueError:
            pass

    server = HTTPServer(("", port), WorkerHandler)
    print(f"worker listening on :{port}", flush=True)
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
