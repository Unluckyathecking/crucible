"""Unit tests for the Python SDK's inbound HMAC-SHA256 channel auth.

Mirrors the signature-verification matrix carried by the other SDKs
(workers/sdk-go/crucible_test.go, sdk-rust/src/server.rs, sdk-ts/src/index.test.ts):
valid, missing, wrong-secret, tampered-body, stale-timestamp, the parser/validation
error branches, and the disabled (no-secret) path.

Run (from workers/sdk-python):

    python3 -m unittest discover -v
"""

from __future__ import annotations

import hashlib
import hmac
import http.client
import json
import threading
import time
import unittest
from http.server import ThreadingHTTPServer
from typing import Dict, Optional, Tuple

import crucible
from crucible import _verify_worker_sig


def _sign(secret: bytes, ts: int, body: bytes) -> str:
    """Build an X-Worker-Signature value: t=<ts>,v1=<hex-hmac-sha256(ts + "." + body)>."""
    ts_str = str(ts)
    mac = hmac.new(secret, digestmod=hashlib.sha256)
    mac.update(ts_str.encode("ascii"))
    mac.update(b".")
    mac.update(body)
    return "t=%s,v1=%s" % (ts_str, mac.hexdigest())


def _now() -> int:
    return int(time.time())


class VerifyWorkerSigTest(unittest.TestCase):
    """Direct coverage of crucible._verify_worker_sig (raises ValueError on any failure)."""

    secret = b"test-shared-secret"
    body = b'{"request_id":"test"}'

    def test_valid_signature_accepted(self) -> None:
        header = _sign(self.secret, _now(), self.body)
        _verify_worker_sig(header, self.body, self.secret)  # must not raise

    def test_missing_signature_rejected(self) -> None:
        with self.assertRaises(ValueError):
            _verify_worker_sig("", self.body, self.secret)

    def test_wrong_secret_rejected(self) -> None:
        header = _sign(b"correct-secret", _now(), self.body)
        with self.assertRaises(ValueError):
            _verify_worker_sig(header, self.body, b"wrong-secret")

    def test_tampered_body_rejected(self) -> None:
        header = _sign(self.secret, _now(), b"original body")
        with self.assertRaises(ValueError):
            _verify_worker_sig(header, b"tampered body", self.secret)

    def test_stale_timestamp_rejected(self) -> None:
        header = _sign(self.secret, _now() - 600, self.body)  # 10 min in the past
        with self.assertRaises(ValueError):
            _verify_worker_sig(header, self.body, self.secret)

    def test_future_timestamp_beyond_window_rejected(self) -> None:
        header = _sign(self.secret, _now() + 600, self.body)  # 10 min in the future
        with self.assertRaises(ValueError):
            _verify_worker_sig(header, self.body, self.secret)

    def test_malformed_header_missing_timestamp(self) -> None:
        with self.assertRaises(ValueError):
            _verify_worker_sig("v1=" + "0" * 64, self.body, self.secret)

    def test_malformed_header_missing_signature(self) -> None:
        with self.assertRaises(ValueError):
            _verify_worker_sig("t=" + str(_now()), self.body, self.secret)

    def test_non_numeric_timestamp(self) -> None:
        with self.assertRaises(ValueError):
            _verify_worker_sig("t=notanumber,v1=" + "0" * 64, self.body, self.secret)

    def test_invalid_hex_signature(self) -> None:
        with self.assertRaises(ValueError):
            _verify_worker_sig("t=%d,v1=not!valid!hex" % _now(), self.body, self.secret)

    def test_wrong_length_signature(self) -> None:
        with self.assertRaises(ValueError):
            _verify_worker_sig("t=%d,v1=deadbeef" % _now(), self.body, self.secret)


def _ok_handler(_req: "crucible.Request") -> "crucible.Response":
    return crucible.Response(payload={"ok": True})


class _Server:
    """A running in-process worker built from create_app with the given shared secret."""

    def __init__(self, secret: str):
        app_cls = crucible.create_app(_ok_handler, crucible.ServerConfig(shared_secret=secret))
        self._httpd = ThreadingHTTPServer(("127.0.0.1", 0), app_cls)
        self.port = self._httpd.server_address[1]
        self._thread = threading.Thread(target=self._httpd.serve_forever, daemon=True)
        self._thread.start()

    def close(self) -> None:
        self._httpd.shutdown()
        self._httpd.server_close()
        self._thread.join(timeout=5)


def _post(port: int, body: bytes, headers: Optional[Dict[str, str]] = None) -> Tuple[int, dict]:
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    try:
        hdrs = {"Content-Type": "application/json"}
        if headers:
            hdrs.update(headers)
        conn.request("POST", "/invoke", body=body, headers=hdrs)
        resp = conn.getresponse()
        return resp.status, json.loads(resp.read())
    finally:
        conn.close()


class SignedInvokeTest(unittest.TestCase):
    """Integration coverage of the signed /invoke path through a running server."""

    def test_signed_request_with_correct_secret_accepted(self) -> None:
        secret = "integration-secret"
        srv = _Server(secret)
        try:
            body = b'{"operation":"test","payload":{}}'
            status, resp = _post(srv.port, body, {"X-Worker-Signature": _sign(secret.encode(), _now(), body)})
            self.assertEqual(status, 200)
            self.assertNotIn("error", resp)
        finally:
            srv.close()

    def test_unsigned_request_rejected_when_secret_configured(self) -> None:
        srv = _Server("integration-secret")
        try:
            status, resp = _post(srv.port, b'{"operation":"test","payload":{}}')
            self.assertEqual(status, 200)
            self.assertEqual((resp.get("error") or {}).get("code"), "UNAUTHORIZED")
        finally:
            srv.close()

    def test_unsigned_request_succeeds_when_no_secret_configured(self) -> None:
        srv = _Server("")  # empty secret disables verification
        try:
            status, resp = _post(srv.port, b'{"operation":"test","payload":{}}')
            self.assertEqual(status, 200)
            self.assertNotIn("error", resp)
        finally:
            srv.close()


if __name__ == "__main__":
    unittest.main()
