"""Crucible worker SDK for Python (stdlib-only, no third-party dependencies).

A worker is an HTTP server with two endpoints:

    POST /invoke   -- handles one invoke request, returns the result + billable_units
    GET  /healthz  -- returns 200 OK when ready

This SDK provides the boilerplate so a complete worker is one function:

    import crucible

    def handler(req: crucible.Request) -> crucible.Response:
        return crucible.Response(payload={"hello": "world"})

    crucible.serve(8081, handler)
"""

from __future__ import annotations

import hashlib
import hmac
import json
import os
import signal
import threading
import time
from dataclasses import dataclass, field
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, Optional, Type

__all__ = [
    "Request",
    "Response",
    "WorkerError",
    "ServerConfig",
    "HandlerFunc",
    "create_app",
    "serve",
]


@dataclass
class Request:
    """Mirrors the InvokeRequest proto for handlers that don't depend on generated proto types."""

    request_id: str = ""
    customer_id: str = ""
    operation: str = ""
    payload: Any = None
    plan: str = ""
    metadata: Dict[str, str] = field(default_factory=dict)

    @classmethod
    def from_dict(cls, data: Dict[str, Any]) -> "Request":
        return cls(
            request_id=data.get("request_id") or "",
            customer_id=data.get("customer_id") or "",
            operation=data.get("operation") or "",
            payload=data.get("payload"),
            plan=data.get("plan") or "",
            metadata=data.get("metadata") or {},
        )


@dataclass
class Response:
    """What a handler returns on success. billable_units is normalised to >= 1 before send."""

    payload: Any
    billable_units: int = 1
    units_label: str = ""

    def to_wire(self) -> Dict[str, Any]:
        # Invariant #2 (billable_units is a contract, not a convention): every
        # successful response must carry billable_units >= 1.
        units = self.billable_units if self.billable_units and self.billable_units >= 1 else 1
        out: Dict[str, Any] = {"payload": self.payload, "billable_units": units}
        if self.units_label:
            out["units_label"] = self.units_label
        return out


class WorkerError(Exception):
    """A structured error a handler can raise to surface a stable code to the customer.

    Handlers may also raise a plain Exception -- the SDK wraps it as a generic
    INTERNAL error (the real cause is logged but never surfaced).
    """

    def __init__(self, code: str, message: str, retryable: bool = False) -> None:
        super().__init__(f"{code}: {message}")
        self.code = code
        self.message = message
        self.retryable = retryable


HandlerFunc = Callable[[Request], Response]


@dataclass
class ServerConfig:
    """Optional configuration for the worker HTTP handler.

    shared_secret enables inbound X-Worker-Signature HMAC-SHA256 verification.
    Empty disables verification (today's behaviour). When create_app()/serve() is
    called without an explicit config, WORKER_SHARED_SECRET from the environment
    is used automatically.
    """

    shared_secret: str = ""


# Header carrying the inbound channel-auth signature.
# Format: t=<unix-seconds>,v1=<hex-sha256-hmac>
# Signing payload: HMAC-SHA256(secret, timestamp + "." + body) -- byte-identical
# to the Go/Rust/TS SDKs so cross-language parity is maintained.
_WORKER_SIG_HEADER = "X-Worker-Signature"

# Maximum age (or future skew) allowed for a signed request, in seconds.
# Mirrors the Stripe webhook replay window used in billing/webhook.go.
_WORKER_SIG_WINDOW = 300

# 10 MiB body cap, mirroring the Go SDK's http.MaxBytesReader limit.
_MAX_BODY_BYTES = 10 * 1024 * 1024


def _verify_worker_sig(header: str, body: bytes, secret: bytes) -> None:
    """Raise ValueError on any verification failure.

    The failure reason is never forwarded to the caller; only UNAUTHORIZED is surfaced.
    """
    if not header:
        raise ValueError("missing signature header")

    ts_str = ""
    sig_hex = ""
    for part in header.split(","):
        if part.startswith("t="):
            ts_str = part[2:]
        elif part.startswith("v1="):
            sig_hex = part[3:]
    if not ts_str or not sig_hex:
        raise ValueError("malformed signature header")

    try:
        ts = int(ts_str)
    except ValueError as exc:
        raise ValueError("invalid timestamp in signature header") from exc

    if abs(int(time.time()) - ts) > _WORKER_SIG_WINDOW:
        raise ValueError("stale timestamp in signature header")

    try:
        provided = bytes.fromhex(sig_hex)
    except ValueError as exc:
        raise ValueError("invalid signature value") from exc
    if len(provided) != hashlib.sha256().digest_size:
        raise ValueError("invalid signature value")

    mac = hmac.new(secret, digestmod=hashlib.sha256)
    mac.update(ts_str.encode("ascii"))
    mac.update(b".")
    mac.update(body)

    # Constant-time comparison, mirroring subtle.ConstantTimeCompare / hmac.Equal.
    if not hmac.compare_digest(provided, mac.digest()):
        raise ValueError("signature mismatch")


def create_app(handler: HandlerFunc, config: Optional[ServerConfig] = None) -> Type[BaseHTTPRequestHandler]:
    """Build a BaseHTTPRequestHandler subclass wired to the frozen worker contract.

    Returns a class (not an instance) suitable for http.server.HTTPServer /
    ThreadingHTTPServer. Use serve() for the standard lifecycle (signal handling,
    graceful shutdown). When config is omitted, WORKER_SHARED_SECRET from the
    environment is used automatically.
    """
    if handler is None:
        raise ValueError("crucible.create_app: handler must not be None")

    cfg = config if config is not None else ServerConfig(shared_secret=os.environ.get("WORKER_SHARED_SECRET", ""))
    secret_bytes = cfg.shared_secret.encode("utf-8") if cfg.shared_secret else b""

    class _WorkerRequestHandler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, format: str, *args: Any) -> None:  # noqa: A002 - stdlib hook signature
            pass  # the framework logs at the gateway layer; suppress default access log noise

        def do_GET(self) -> None:
            if self.path == "/healthz":
                self._healthz()
            elif self.path == "/invoke":
                self._method_not_allowed()
            else:
                self._not_found()

        def do_POST(self) -> None:
            if self.path != "/invoke":
                self._not_found()
                return
            self._handle_invoke()

        def do_HEAD(self) -> None:
            self._reject_or_404()

        def do_PUT(self) -> None:
            self._reject_or_404()

        def do_DELETE(self) -> None:
            self._reject_or_404()

        def do_PATCH(self) -> None:
            self._reject_or_404()

        def do_OPTIONS(self) -> None:
            self._reject_or_404()

        def _reject_or_404(self) -> None:
            if self.path == "/invoke":
                self._method_not_allowed()
            else:
                self._not_found()

        def _healthz(self) -> None:
            body = b'{"status":"ok"}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def _not_found(self) -> None:
            self.send_response(404)
            self.send_header("Content-Length", "0")
            self.end_headers()

        def _method_not_allowed(self) -> None:
            # 405 per the frozen contract (POST-only /invoke).
            self.send_response(405)
            self.send_header("Allow", "POST")
            self.send_header("Content-Length", "0")
            self.end_headers()

        def _handle_invoke(self) -> None:
            try:
                length = int(self.headers.get("Content-Length") or 0)
            except ValueError:
                length = 0

            if length > _MAX_BODY_BYTES:
                self._write_error(WorkerError("BAD_REQUEST", "request body too large"))
                self.close_connection = True
                return

            raw_body = self.rfile.read(length) if length > 0 else b""

            # Verify the HMAC-SHA256 channel-auth signature when configured.
            # Empty secret_bytes -> verification disabled -> today's behaviour preserved.
            if secret_bytes:
                try:
                    _verify_worker_sig(self.headers.get(_WORKER_SIG_HEADER, ""), raw_body, secret_bytes)
                except ValueError:
                    # Surface only a stable code; the signature detail is never echoed.
                    self._write_error(WorkerError("UNAUTHORIZED", "invalid request signature"))
                    return

            try:
                parsed = json.loads(raw_body) if raw_body else None
                if not isinstance(parsed, dict):
                    raise ValueError("request body must be a JSON object")
            except (json.JSONDecodeError, ValueError):
                self._write_error(WorkerError("BAD_REQUEST", "invalid request body"))
                return

            req = Request.from_dict(parsed)
            try:
                resp = handler(req)
            except WorkerError as err:
                self._write_error(err)
                return
            except Exception:
                # The real cause is intentionally swallowed here; only a generic
                # code is surfaced to the customer (matches the Go/Rust/TS SDKs).
                self._write_error(WorkerError("INTERNAL", "internal error", retryable=True))
                return

            self._write_json(200, resp.to_wire())

        def _write_json(self, status: int, obj: Dict[str, Any]) -> None:
            data = json.dumps(obj).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def _write_error(self, err: WorkerError) -> None:
            self._write_json(
                200,
                {"error": {"code": err.code, "message": err.message, "retryable": err.retryable}},
            )

    return _WorkerRequestHandler


def serve(port: int, handler: HandlerFunc, config: Optional[ServerConfig] = None) -> None:
    """Run the worker HTTP server on the given port and block until SIGINT/SIGTERM.

    Mirrors crucible.Serve (Go) / serve() (TS): logs a startup line, blocks, then
    stops accepting new connections and closes the listening socket on shutdown.
    """
    app_cls = create_app(handler, config)
    httpd = ThreadingHTTPServer(("", port), app_cls)
    httpd.daemon_threads = True

    def _handle_signal(signum: int, frame: Any) -> None:
        # shutdown() must not be called from the thread running serve_forever().
        threading.Thread(target=httpd.shutdown, daemon=True).start()

    signal.signal(signal.SIGINT, _handle_signal)
    signal.signal(signal.SIGTERM, _handle_signal)

    print(json.dumps({"level": "info", "port": port, "msg": "worker listening"}), flush=True)
    try:
        httpd.serve_forever()
    finally:
        print(json.dumps({"level": "info", "msg": "worker shutting down"}), flush=True)
        httpd.server_close()
