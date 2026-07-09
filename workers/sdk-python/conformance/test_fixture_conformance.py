"""Fixture-driven conformance tests for the Crucible Python SDK.

Loads workers/conformance/fixture.json (the language-neutral spec) and asserts
each case against an in-process server built from crucible.create_app(), mirroring
workers/sdk-ts/conformance/conformance.test.js and workers/sdk-go/conformance/fixture_test.go.
The fixture is the single source of truth; adding a case means adding it to the
JSON -- no per-SDK-only test cases.

Run (from workers/sdk-python):

    python3 -m unittest conformance.test_fixture_conformance -v
"""

from __future__ import annotations

import http.client
import json
import os
import threading
import unittest
from http.server import ThreadingHTTPServer
from typing import Any, Dict, Optional, Tuple

import crucible

_FIXTURE_PATH = os.path.join(os.path.dirname(__file__), "..", "..", "conformance", "fixture.json")


def _load_fixture_cases() -> list:
    with open(_FIXTURE_PATH, "r", encoding="utf-8") as f:
        data = json.load(f)
    cases = data.get("cases") or []
    if not cases:
        raise AssertionError(f"shared fixture loaded zero cases; check {_FIXTURE_PATH}")
    return cases


def _echo_handler(_req: "crucible.Request") -> "crucible.Response":
    return crucible.Response(payload={"ok": True}, billable_units=1)


class _EchoServer:
    """A running in-process worker server backed by the given handler (echo by default)."""

    def __init__(self, handler=_echo_handler):
        app_cls = crucible.create_app(handler, crucible.ServerConfig())
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), app_cls)
        self.port = self._httpd.server_address[1]
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=5)


def _request(
    port: int, method: str, path: str, body: Optional[str] = None, headers: Optional[Dict[str, str]] = None
) -> Tuple[int, Dict[str, str], bytes]:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    try:
        hdrs = dict(headers or {})
        if body is not None:
            hdrs.setdefault("Content-Type", "application/json")
        conn.request(method, path, body=body, headers=hdrs)
        resp = conn.getresponse()
        raw = resp.read()
        return resp.status, {k: v for k, v in resp.getheaders()}, raw
    finally:
        conn.close()


# ── Per-case assertion helpers, one per fixture "kind" ────────────────────────


def _assert_healthz_body(test: unittest.TestCase, port: int) -> None:
    status, headers, raw = _request(port, "GET", "/healthz")
    test.assertEqual(status, 200, "healthz must return HTTP 200")
    ct = headers.get("Content-Type", "")
    test.assertTrue(ct.startswith("application/json"), f"healthz Content-Type must be application/json, got {ct!r}")
    # Byte-exact comparison so trailing whitespace or pretty-printing is caught.
    test.assertEqual(raw, b'{"status":"ok"}', f"healthz body must be exactly {{\"status\":\"ok\"}}, got {raw!r}")


def _assert_method_not_allowed(test: unittest.TestCase, port: int) -> None:
    for method in ("GET", "HEAD", "PUT", "DELETE", "PATCH", "OPTIONS"):
        status, _headers, _raw = _request(port, method, "/invoke")
        test.assertEqual(status, 405, f"{method} /invoke must return 405")


def _assert_billable_units_floor(test: unittest.TestCase, _port: int) -> None:
    def handler(_req: "crucible.Request") -> "crucible.Response":
        return crucible.Response(payload={"floor": "ok"}, billable_units=0)

    srv = _EchoServer(handler)
    try:
        status, _headers, raw = _request(
            srv.port, "POST", "/invoke", body=json.dumps({"operation": "floor_test", "payload": {}})
        )
        test.assertEqual(status, 200)
        body: Dict[str, Any] = json.loads(raw)
        units = body.get("billable_units")
        test.assertIsInstance(units, int, f"billable_units must be a JSON number, got {type(units)}")
        test.assertGreaterEqual(units, 1, f"billable_units must be >= 1 after SDK normalisation, got {units}")
    finally:
        srv.close()


def _assert_apierror_envelope(test: unittest.TestCase, _port: int) -> None:
    error_code = "FIXTURE_TEST_ERROR"

    def handler(_req: "crucible.Request") -> "crucible.Response":
        raise crucible.WorkerError(error_code, "fixture-driven error test", retryable=True)

    srv = _EchoServer(handler)
    try:
        status, _headers, raw = _request(
            srv.port, "POST", "/invoke", body=json.dumps({"operation": "err_test", "payload": {}})
        )
        test.assertEqual(status, 200, "error envelopes must return HTTP 200")
        body: Dict[str, Any] = json.loads(raw)
        test.assertIn("error", body, "error field must be present")
        test.assertEqual(body["error"]["code"], error_code)
        test.assertTrue(body["error"]["message"], "error.message must be non-empty")
        test.assertIsInstance(body["error"]["retryable"], bool, "error.retryable must be a boolean")
        test.assertNotIn("payload", body, "error envelope must not contain payload key")
        test.assertNotIn("billable_units", body, "error envelope must not contain billable_units key")
    finally:
        srv.close()


def _assert_empty_body_bad_request(test: unittest.TestCase, port: int) -> None:
    # Empty body is not valid JSON; the SDK must return a BAD_REQUEST error envelope.
    status, _headers, raw = _request(port, "POST", "/invoke", body="")
    test.assertEqual(status, 200, "error envelopes always return HTTP 200")
    body: Dict[str, Any] = json.loads(raw)
    test.assertEqual(
        (body.get("error") or {}).get("code"), "BAD_REQUEST", f"empty body must yield BAD_REQUEST, got {body!r}"
    )


_CASE_ASSERTIONS = {
    "healthz_body": _assert_healthz_body,
    "method_not_allowed": _assert_method_not_allowed,
    "billable_units_floor": _assert_billable_units_floor,
    "apierror_envelope": _assert_apierror_envelope,
    "empty_body_bad_request": _assert_empty_body_bad_request,
}


def _make_case_test(case: Dict[str, Any]):
    known_divergence = (case.get("known_divergences") or {}).get("python")

    def run(self: "FixtureCasesTest") -> None:
        if known_divergence:
            self.skipTest(f"known Python divergence: {known_divergence.get('note')}")
        assertion = _CASE_ASSERTIONS.get(case["kind"])
        if assertion is None:
            self.fail(f"unknown fixture case kind {case['kind']!r} (id={case['id']}): update _CASE_ASSERTIONS")
        assertion(self, self.server.port)

    run.__name__ = f"test_{case['id']}"
    run.__doc__ = case.get("description", case["id"])
    return run


class FixtureCasesTest(unittest.TestCase):
    """One generated test method per workers/conformance/fixture.json case."""

    server: _EchoServer

    @classmethod
    def setUpClass(cls) -> None:
        cls.server = _EchoServer()

    @classmethod
    def tearDownClass(cls) -> None:
        cls.server.close()


for _case in _load_fixture_cases():
    setattr(FixtureCasesTest, f"test_{_case['id']}", _make_case_test(_case))


if __name__ == "__main__":
    unittest.main()
