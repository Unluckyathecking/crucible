"""
Self-contained smoke test for the Python Crucible worker stub.

Starts the server in a background thread, runs HTTP assertions, then shuts down.
No external dependencies — stdlib only.

Run:  python3 test_worker.py
  or: python3 -m pytest test_worker.py -v
"""

import json
import socket
import threading
import time
import urllib.error
import urllib.request
from http.server import HTTPServer

import pytest

from worker import Handler


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("", 0))
        return s.getsockname()[1]


class _Server:
    def __init__(self):
        self.port = _free_port()
        self._httpd = HTTPServer(("127.0.0.1", self.port), Handler)
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)

    def start(self):
        self._thread.start()
        # wait until the port accepts connections
        deadline = time.monotonic() + 3
        while time.monotonic() < deadline:
            try:
                with socket.create_connection(("127.0.0.1", self.port), timeout=0.1):
                    break
            except OSError:
                time.sleep(0.05)

    def stop(self):
        self._httpd.shutdown()

    def get(self, path: str) -> dict:
        url = f"http://127.0.0.1:{self.port}{path}"
        with urllib.request.urlopen(url) as r:
            return json.loads(r.read())

    def post(self, path: str, body: dict) -> dict:
        data = json.dumps(body).encode()
        req = urllib.request.Request(
            f"http://127.0.0.1:{self.port}{path}",
            data=data,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(req) as r:
            return json.loads(r.read())


@pytest.fixture(scope="module")
def server():
    s = _Server()
    s.start()
    yield s
    s.stop()


def test_healthz(server):
    resp = server.get("/healthz")
    assert resp == {"status": "ok"}


def test_invoke_envelope_shape(server):
    resp = server.post("/invoke", {"operation": "echo", "payload": {"x": "hi"}, "metadata": {}})
    assert "payload" in resp
    assert "billable_units" in resp


def test_invoke_billable_units_default_one(server):
    resp = server.post("/invoke", {"operation": "echo", "payload": {}, "metadata": {}})
    assert resp["billable_units"] >= 1


def test_invoke_billable_units_from_metadata(server):
    resp = server.post(
        "/invoke",
        {"operation": "echo", "payload": {"x": "hi"}, "metadata": {"units": "3"}},
    )
    assert resp["billable_units"] == 3


def test_invoke_units_bad_value_falls_back_to_one(server):
    resp = server.post("/invoke", {"operation": "echo", "payload": {}, "metadata": {"units": "abc"}})
    assert resp["billable_units"] >= 1


def test_invoke_echoes_payload(server):
    resp = server.post(
        "/invoke",
        {"operation": "echo", "payload": {"key": "value"}, "metadata": {}},
    )
    assert resp["payload"]["echo"] == {"key": "value"}
    assert resp["payload"]["operation"] == "echo"


def test_invoke_bad_json(server):
    port = server.port
    data = b"not json"
    req = urllib.request.Request(
        f"http://127.0.0.1:{port}/invoke",
        data=data,
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    with urllib.request.urlopen(req) as r:
        resp = json.loads(r.read())
    assert "error" in resp


if __name__ == "__main__":
    # Allow running as a plain script without pytest
    import sys

    srv = _Server()
    srv.start()
    try:
        tests = [
            test_healthz,
            test_invoke_envelope_shape,
            test_invoke_billable_units_default_one,
            test_invoke_billable_units_from_metadata,
            test_invoke_units_bad_value_falls_back_to_one,
            test_invoke_echoes_payload,
            test_invoke_bad_json,
        ]
        failed = 0
        for t in tests:
            try:
                t(srv)
                print(f"  PASS  {t.__name__}")
            except Exception as exc:
                print(f"  FAIL  {t.__name__}: {exc}")
                failed += 1
        print(f"\n{len(tests) - failed}/{len(tests)} passed")
        sys.exit(1 if failed else 0)
    finally:
        srv.stop()
