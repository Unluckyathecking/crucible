#!/usr/bin/env python3
"""Hello-world Crucible worker stub (Python).

Every Python worker in a Crucible clone starts from this shape: handle /invoke
and /healthz directly using only the standard library (no dependencies to install).
Per-product logic lives entirely in the invoke() function body.

Run locally:
    python3 worker.py          # listens on $PORT or 8081

Smoke test:
    curl http://localhost:8081/healthz
    curl -X POST http://localhost:8081/invoke \
         -H 'content-type: application/json' \
         -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{},"plan":"free"}'
"""
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, HTTPServer


def invoke(request: dict) -> dict:
    """Echo handler — mirrors the Go stub. Returns payload + operation back.

    The billable_units field MUST be >= 1 on every successful response.
    The gateway enforces this and returns 502 WORKER_BAD_RESPONSE otherwise.
    """
    units = 1
    raw_units = (request.get("metadata") or {}).get("units")
    if raw_units is not None:
        try:
            n = int(raw_units)
            if n >= 1:
                units = n
        except (ValueError, TypeError):
            pass
    return {
        "payload": {"echo": request.get("payload"), "operation": request.get("operation")},
        "billable_units": units,
        "units_label": "",
    }


class Handler(BaseHTTPRequestHandler):
    def log_message(self, format, *args):  # noqa: A002
        # Suppress default access log; the framework logs at the gateway layer.
        pass

    def do_GET(self):
        if self.path == "/healthz":
            self._respond(200, b'{"status":"ok"}')
        else:
            self._respond(404, b"not found")

    def do_POST(self):
        if self.path != "/invoke":
            self._respond(404, b"not found")
            return
        length = int(self.headers.get("content-length", 0))
        body = self.rfile.read(length)
        try:
            req = json.loads(body)
        except json.JSONDecodeError as exc:
            self._respond(200, json.dumps({"error": {"code": "BAD_REQUEST", "message": str(exc), "retryable": False}}, separators=(',', ':')).encode())
            return
        result = invoke(req)
        self._respond(200, json.dumps(result, separators=(',', ':')).encode())

    def _respond(self, status: int, body: bytes):
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def main():
    raw_port = os.environ.get("PORT", "8081")
    try:
        port = int(raw_port)
    except ValueError:
        print(f"warning: invalid PORT {raw_port!r}, using default 8081", file=sys.stderr, flush=True)
        port = 8081
    server = HTTPServer(("", port), Handler)
    server.serve_forever()


if __name__ == "__main__":
    main()
