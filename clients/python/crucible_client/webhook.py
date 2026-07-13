"""webhook.py provides verify_webhook, a signature verifier for Crucible gateway
webhook deliveries. Hand-maintained — NOT written by scripts/gen-clients.sh
(mirrors clients/go/webhook.go and clients/typescript/src/webhook.ts, which are
also excluded from their respective generators' write scope).

Algorithm must stay byte-identical to the Go/TS implementations: same
HMAC-SHA256 construction (timestamp + "." + body), same 5-minute default
tolerance window, same constant-time comparison (hmac.compare_digest).
"""
from __future__ import annotations

import hashlib
import hmac
import math
import re
import time
from typing import List, Tuple

# MAX_SIG_CANDIDATES caps how many v1= values are parsed and compared. Mirrors
# the gateway verifySignature defense: an attacker cannot force unbounded HMAC
# comparisons by stuffing the header with many candidates.
MAX_SIG_CANDIDATES = 8

# MAX_HEADER_PARTS caps the total comma-separated segments to prevent unbounded
# iteration over attacker-controlled input before the v1 candidate cap applies.
MAX_HEADER_PARTS = 16

# SHA-256 produces 32 bytes; hex-encoded that is 64 characters.
_SHA256_BYTE_LEN = 32
_SHA256_HEX_LEN = _SHA256_BYTE_LEN * 2

# Rejects any character bytes.fromhex() wouldn't reject on its own, notably
# ASCII whitespace — see the comment at its call site in verify_webhook.
_SECRET_HEX_CONTENT_RE = re.compile(r"^[0-9a-fA-F]+$")

# 15 digits comfortably covers every real Unix timestamp while bounding the
# string int() has to parse. Uses [0-9], not \d: unlike JavaScript's \d (always
# ASCII-only), Python's \d also matches non-ASCII Unicode decimal digits (e.g.
# Arabic-Indic) by default — those pass int() too but then fail
# timestamp.encode("ascii") below with a raw UnicodeEncodeError instead of the
# typed WebhookVerificationError, so this must reject them at the regex stage.
_TIMESTAMP_RE = re.compile(r"^[0-9]{1,15}$")

#: Default tolerance in ms: 5 minutes, matching the gateway's inbound replay window.
DEFAULT_TOLERANCE_MS = 5 * 60 * 1000

#: HTTP header carrying the t= timestamp and v1= HMAC digest.
SIGNATURE_HEADER = "X-Crucible-Signature"

#: HTTP header carrying the delivery Unix timestamp.
TIMESTAMP_HEADER = "X-Crucible-Timestamp"

#: HTTP header carrying the delivery UUID. Use it to deduplicate at-least-once deliveries.
WEBHOOK_EVENT_ID_HEADER = "X-Webhook-Event-ID"

#: HTTP header carrying the event type string (e.g. "invoice.paid").
WEBHOOK_EVENT_TYPE_HEADER = "X-Webhook-Event-Type"


class WebhookVerificationError(Exception):
    """Raised when X-Crucible-Signature verification fails."""


def verify_webhook(
    secret_hex: str,
    sig_header: str,
    body: bytes,
    tolerance_ms: "int | None" = None,
) -> None:
    """Verify the X-Crucible-Signature on a webhook delivery from the gateway.

    secret_hex is the hex-encoded signing secret shown on the dashboard endpoint
    page. sig_header is the raw value of the X-Crucible-Signature header
    (format: t=<ts>,v1=<hex>). body is the unmodified request body bytes.
    tolerance_ms is the maximum accepted age in milliseconds; omit or pass None
    to use DEFAULT_TOLERANCE_MS. Unlike the Go SDK (whose Duration zero value
    doubles as a "use default" sentinel), an explicit 0 here means zero-width
    tolerance, not the default — mirroring the TypeScript SDK's convention.
    Raises WebhookVerificationError; a raised error means the payload must not
    be trusted.
    """
    # Catches the common misconfiguration of passing a decoded str instead of
    # the raw request bytes — mirrors the TypeScript Buffer.isBuffer guard.
    if not isinstance(body, (bytes, bytearray)):
        raise WebhookVerificationError(
            "body must be bytes; pass raw request bytes before any parsing"
        )

    if tolerance_ms is None:
        tolerance_ms = DEFAULT_TOLERANCE_MS
    # math.isfinite rejects NaN/inf float values a caller might pass despite the
    # int type hint; PEP 484 hints are not enforced at runtime.
    if not math.isfinite(tolerance_ms):
        raise WebhookVerificationError("tolerance_ms must be a finite number")
    if tolerance_ms < 0:
        raise WebhookVerificationError("negative tolerance not allowed")

    # Two-stage validation (length, then hex-content) mirrors Go's
    # len/DecodeString split — each stage raises a distinct message so callers
    # (and tests) can tell a length problem from a content problem.
    if len(secret_hex) == 0 or len(secret_hex) % 2 != 0:
        raise WebhookVerificationError(
            "invalid secret_hex: must be non-empty even-length hex string"
        )
    # bytes.fromhex() silently skips ASCII whitespace between byte pairs (a
    # documented stdlib quirk neither Go's hex.DecodeString nor the regex-
    # gated TS path shares) — a secret_hex that's all or partly whitespace
    # would otherwise decode to a shorter, degenerate key instead of failing,
    # so hex-content is validated explicitly before decoding.
    if not _SECRET_HEX_CONTENT_RE.match(secret_hex):
        raise WebhookVerificationError(
            "invalid secret_hex: contains non-hex characters"
        )
    secret = bytes.fromhex(secret_hex)

    # Capture clock before parsing attacker-controlled header content, mirroring
    # both Go's and TypeScript's placement (immediately ahead of the timestamp
    # comparisons that consume it).
    now_ms = time.time_ns() // 1_000_000

    timestamp, sigs = _parse_signature_header(sig_header)

    if not _TIMESTAMP_RE.match(timestamp):
        raise WebhookVerificationError("bad timestamp in signature header")
    # Reject leading zeros on multi-digit timestamps (e.g. "01234"): valid Unix
    # timestamps never round-trip through str() with a leading zero, so this
    # would never match a genuine gateway-issued value. Single-digit "0" is
    # allowed (len==1 fails the len>1 condition below).
    if len(timestamp) > 1 and timestamp[0] == "0":
        raise WebhookVerificationError("bad timestamp in signature header")
    ts = int(timestamp)
    ts_ms = ts * 1000

    if ts_ms > now_ms:
        raise WebhookVerificationError("webhook timestamp in the future")
    age_ms = now_ms - ts_ms  # always >= 0 because now_ms >= ts_ms
    if age_ms > tolerance_ms:
        raise WebhookVerificationError("webhook timestamp too old (replay protection)")

    mac = hmac.new(secret, digestmod=hashlib.sha256)
    mac.update(timestamp.encode("ascii"))
    mac.update(b".")
    mac.update(bytes(body))
    expected = mac.digest()

    for sig_hex in sigs:
        # Length guard mirrors the Go/TS len(sigHex) != 64 check: reject
        # wrong-length inputs on the fast path before decoding.
        if len(sig_hex) != _SHA256_HEX_LEN:
            continue
        try:
            candidate = bytes.fromhex(sig_hex)
        except ValueError:
            continue
        if len(candidate) != _SHA256_BYTE_LEN:
            continue
        # hmac.compare_digest is timing-safe (implemented in C; does not
        # short-circuit on the first differing byte).
        if hmac.compare_digest(candidate, expected):
            return
    raise WebhookVerificationError("no matching v1 signature")


def _parse_signature_header(header: str) -> Tuple[str, List[str]]:
    if not header:
        raise WebhookVerificationError("missing X-Crucible-Signature header")
    # maxsplit=MAX_HEADER_PARTS yields at most MAX_HEADER_PARTS+1 parts, with any
    # excess comma-separated text folded into the final element (Python's
    # str.split(sep, maxsplit) matches Go's strings.SplitN semantics here, not
    # TypeScript's String.prototype.split(sep, limit), which instead drops the
    # remainder — either way, a header with too many segments produces a parts
    # list of length MAX_HEADER_PARTS+1 and is rejected by the check below).
    parts = header.split(",", MAX_HEADER_PARTS)
    if len(parts) > MAX_HEADER_PARTS:
        raise WebhookVerificationError("malformed X-Crucible-Signature header")

    timestamp = ""
    sigs: List[str] = []
    for part in parts:
        idx = part.find("=")
        # idx <= 0 rejects both a missing "=" (idx == -1) and an empty key
        # (idx == 0, e.g. "=abc"). The second find rejects an embedded "=" in
        # the value (e.g. "t=1=2") — none of the current key types allow it,
        # and accepting it creates parser ambiguity for future extensions.
        if idx <= 0 or part.find("=", idx + 1) != -1:
            raise WebhookVerificationError("malformed X-Crucible-Signature header")
        key = part[:idx]
        val = part[idx + 1 :]
        # Reject empty values universally (e.g. "t=", "v1=", "foo=") — applies
        # to every key including unknown ones, for cross-language parity.
        if val == "":
            raise WebhookVerificationError("malformed X-Crucible-Signature header")
        if key == "t":
            # Exactly one timestamp per delivery: duplicate t= is invalid.
            if timestamp != "":
                raise WebhookVerificationError("malformed X-Crucible-Signature header")
            timestamp = val
        elif key == "v1":
            # Multiple v1= values are accepted intentionally: during secret
            # rotation the gateway may include two signatures (old key + new
            # key). Excess candidates beyond MAX_SIG_CANDIDATES are silently
            # dropped, not rejected — rejecting would let an attacker DoS
            # receivers by prepending junk v1= values ahead of a legitimate one.
            # The loop keeps running to completion regardless of the cap, so a
            # duplicate t= (or other malformed field) appearing after the cap
            # is still caught.
            if len(sigs) < MAX_SIG_CANDIDATES:
                sigs.append(val)
        # Unknown keys with non-empty values (e.g. future v2=...) are silently
        # ignored for forward compatibility.

    if timestamp == "" or not sigs:
        raise WebhookVerificationError("malformed X-Crucible-Signature header")
    return timestamp, sigs
