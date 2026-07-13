"""Hand-maintained tests for crucible_client.webhook.verify_webhook.

Mirrors clients/go/webhook_test.go and clients/typescript/test/webhook.test.ts —
same reference vectors and edge cases, ported to pytest idioms.
"""
from __future__ import annotations

import hashlib
import hmac
import time

import pytest

from crucible_client.webhook import (
    DEFAULT_TOLERANCE_MS,
    SIGNATURE_HEADER,
    TIMESTAMP_HEADER,
    WEBHOOK_EVENT_ID_HEADER,
    WEBHOOK_EVENT_TYPE_HEADER,
    WebhookVerificationError,
    verify_webhook,
)

SHA256_HEX_LEN = hashlib.sha256().digest_size * 2


def sign(secret: bytes, timestamp: str, body: bytes) -> str:
    """Algorithm-equivalent to gateway/internal/webhookout.Sign (same update
    order, same "." separator). Independent of verify_webhook's own HMAC call —
    test_known_good_vector cross-checks against a pre-computed digest so this
    helper cannot silently drift from the gateway signer undetected."""
    mac = hmac.new(secret, digestmod=hashlib.sha256)
    mac.update(timestamp.encode("ascii"))
    mac.update(b".")
    mac.update(body)
    return mac.digest().hex()


def now_ts() -> str:
    """2 minutes in the past absorbs scheduling jitter without approaching the
    5-minute default tolerance used by most tests."""
    return str(int(time.time()) - 120)


def expect_error(fn, *args, **kwargs) -> WebhookVerificationError:
    with pytest.raises(WebhookVerificationError) as exc_info:
        fn(*args, **kwargs)
    return exc_info.value


def test_known_good_vector():
    # Pre-computed reference vector — independent of sign(). Shared across all
    # three SDKs to catch algorithmic drift from the gateway signer.
    # secret=0x00*32, timestamp="1700000000" (2023-11-14), body={"event":"test"}
    # HMAC-SHA256 = 247d0f12bc3bef311cdb44ced37a1192ba82e78ffe8edd22fbf2205a414e94f5
    secret_hex = "00" * 32
    body = b'{"event":"test"}'
    header = "t=1700000000,v1=247d0f12bc3bef311cdb44ced37a1192ba82e78ffe8edd22fbf2205a414e94f5"

    vector_age_ms = int(time.time() * 1000) - 1700000000 * 1000
    if vector_age_ms < 0:
        pytest.skip("system clock predates reference vector")
    verify_webhook(secret_hex, header, body, vector_age_ms + 3600_000)

    # The same vector must be rejected under DEFAULT_TOLERANCE_MS: it's from
    # 2023, so replay protection must fire.
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "too old" in str(err)


def test_valid_signature():
    secret = bytes(range(1, 33))
    secret_hex = secret.hex()
    body = b'{"event":"delivery.succeeded","data":{"id":1}}'
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_default_tolerance_when_omitted():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    verify_webhook(secret_hex, header, body)


def test_explicit_zero_tolerance_is_zero_width_not_default():
    # Unlike Go (zero Duration doubles as the "use default" sentinel), an
    # explicit 0 here means zero-width tolerance — mirrors the TypeScript SDK.
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()  # 2 minutes in the past
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, 0)
    assert "too old" in str(err)


def test_rejects_tampered_body():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"original"}'
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(
        verify_webhook, secret_hex, header, b'{"event":"tampered"}', 30 * 60 * 1000
    )
    assert "no matching v1 signature" in str(err)


def test_rejects_wrong_secret():
    correct_secret = bytes([0xAA] * 32)
    wrong_secret = bytes([0xBB] * 32)
    secret_hex = correct_secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    header = f"t={ts},v1={sign(wrong_secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "no matching v1 signature" in str(err)


def test_rejects_expired_timestamp():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = str(int(time.time()) - 600)
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "too old" in str(err)


def test_rejects_future_timestamp():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = str(int(time.time()) + 600)
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "future" in str(err)


def test_accepts_second_v1_candidate_when_first_is_invalid():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"multi"}'
    ts = now_ts()
    valid_sig = sign(secret, ts, body)
    invalid_sig = "a" * SHA256_HEX_LEN
    header = f"t={ts},v1={invalid_sig},v1={valid_sig}"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_rejects_valid_sig_beyond_max_sig_candidates():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    valid_sig = sign(secret, ts, body)
    fake_sigs = ",".join(f"v1={'b' * SHA256_HEX_LEN}" for _ in range(8))
    header = f"t={ts},{fake_sigs},v1={valid_sig}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "no matching v1 signature" in str(err)


def test_accepts_exactly_max_sig_candidates():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    valid_sig = sign(secret, ts, body)
    fake_sigs = ",".join(f"v1={'b' * SHA256_HEX_LEN}" for _ in range(7))
    header = f"t={ts},{fake_sigs},v1={valid_sig}"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_missing_header():
    err = expect_error(verify_webhook, "aabb", "", b"body", DEFAULT_TOLERANCE_MS)
    assert "missing" in str(err)


@pytest.mark.parametrize(
    "secret,want_msg",
    [
        ("", "must be non-empty"),
        ("zz", "non-hex"),
        ("abc", "even-length"),
    ],
)
def test_invalid_secret_hex(secret, want_msg):
    ts = now_ts()
    header = f"t={ts},v1={'a' * SHA256_HEX_LEN}"
    err = expect_error(verify_webhook, secret, header, b'{"event":"test"}', DEFAULT_TOLERANCE_MS)
    assert want_msg in str(err)


@pytest.mark.parametrize(
    "secret",
    [
        "  ",  # all whitespace, even length
        "aa  bb" + "cc" * 13,  # embedded double-space, even length overall
        "a\n",  # trailing newline, even total length — match() + $ lets this
        # through even without re.MULTILINE (Python's $ matches just before a
        # trailing newline); fullmatch() is required to reject it.
    ],
)
def test_secret_hex_rejects_whitespace(secret):
    # bytes.fromhex() silently skips ASCII whitespace between byte pairs — a
    # stdlib quirk neither Go's hex.DecodeString nor the TS regex path share.
    # Without an explicit content check, an all-or-partly-whitespace
    # secret_hex would decode to a shorter, degenerate key instead of failing.
    ts = now_ts()
    header = f"t={ts},v1={'a' * SHA256_HEX_LEN}"
    err = expect_error(verify_webhook, secret, header, b'{"event":"test"}', DEFAULT_TOLERANCE_MS)
    assert "non-hex" in str(err)


def test_uppercase_secret_hex():
    secret = bytes(range(1, 33))
    secret_hex_upper = secret.hex().upper()
    body = b'{"event":"test"}'
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    verify_webhook(secret_hex_upper, header, body, DEFAULT_TOLERANCE_MS)


def test_negative_tolerance():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, -60_000)
    assert "negative tolerance" in str(err)


def test_non_finite_tolerance():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    for bad in (float("nan"), float("inf"), float("-inf")):
        err = expect_error(verify_webhook, secret_hex, header, body, bad)
        assert "finite" in str(err)


def test_empty_body():
    secret = bytes([0x11] * 32)
    secret_hex = secret.hex()
    body = b""
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_rejects_non_bytes_body():
    secret = bytes(32)
    secret_hex = secret.hex()
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, b'x')}"
    err = expect_error(verify_webhook, secret_hex, header, "x", DEFAULT_TOLERANCE_MS)
    assert "must be bytes" in str(err)


def test_malformed_header_no_timestamp():
    secret_hex = bytes(32).hex()
    body = b'{"event":"test"}'
    header = f"v1={'a' * SHA256_HEX_LEN}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_malformed_header_no_signature():
    secret_hex = bytes(32).hex()
    body = b'{"event":"test"}'
    header = f"t={now_ts()}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


@pytest.mark.parametrize(
    "bad_ts,want_msg",
    [
        ("abc", "bad timestamp"),
        ("1.5", "bad timestamp"),
        ("0x10", "bad timestamp"),
        ("", "malformed"),
        ("0123456789", "bad timestamp"),
        ("+1234567890", "bad timestamp"),
        ("-1", "bad timestamp"),
        ("1000000000000000", "bad timestamp"),  # 16 digits, exceeds the 15-char bound
    ],
)
def test_malformed_timestamp(bad_ts, want_msg):
    secret_hex = bytes(32).hex()
    body = b'{"event":"test"}'
    header = f"t={bad_ts},v1={'a' * SHA256_HEX_LEN}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert want_msg in str(err)


def test_ancient_timestamp():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = "946684800"  # 2000-01-01 UTC — far beyond any tolerance window
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "too old" in str(err)


def test_epoch_timestamp():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = "0"  # valid decimal, no leading-zero issue, but far outside any window
    header = f"t={ts},v1={sign(secret, ts, body)}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "too old" in str(err)


def test_max_header_parts_exceeded():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    sig = sign(secret, ts, body)
    filler = ",".join(f"x{i}=y" for i in range(15))
    header = f"t={ts},v1={sig},{filler}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_max_header_parts_at_boundary():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    sig = sign(secret, ts, body)
    filler = ",".join(f"x{i}=y" for i in range(14))
    header = f"t={ts},v1={sig},{filler}"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_v1_too_long():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    valid_sig = sign(secret, ts, body)
    header = f"t={ts},v1={valid_sig}00"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "no matching v1 signature" in str(err)


def test_v1_too_short():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    short_sig = "a" * (SHA256_HEX_LEN // 2)
    header = f"t={ts},v1={short_sig}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "no matching v1 signature" in str(err)


def test_v1_non_hex_chars():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    non_hex_sig = "g" + "0" * (SHA256_HEX_LEN - 1)
    header = f"t={ts},v1={non_hex_sig}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "no matching v1 signature" in str(err)


@pytest.mark.parametrize("name,header_tail", [
    ("real_first", None),
    ("attacker_first", None),
])
def test_duplicate_timestamp(name, header_tail):
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    sig = sign(secret, ts, body)
    if name == "real_first":
        header = f"t={ts},t=999,v1={sig}"
    else:
        header = f"t=999,t={ts},v1={sig}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_empty_v1_value():
    secret_hex = bytes(32).hex()
    body = b'{"event":"test"}'
    header = f"t={now_ts()},v1="
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_empty_key():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    sig = sign(secret, ts, body)
    header = f"t={ts},v1={sig},=extra"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_unknown_key_empty_value():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    sig = sign(secret, ts, body)
    header = f"t={ts},v1={sig},foo="
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_unknown_key_forward_compat():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = b'{"event":"test"}'
    ts = now_ts()
    sig = sign(secret, ts, body)
    header = f"t={ts},v1={sig},foo=bar"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_embedded_equal_in_timestamp_value():
    secret_hex = bytes(32).hex()
    body = b'{"event":"test"}'
    header = f"t=1=2,v1={'a' * SHA256_HEX_LEN}"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_embedded_equal_in_v1_value():
    secret_hex = bytes(32).hex()
    body = b'{"event":"test"}'
    header = f"t={now_ts()},v1=ab=cd"
    err = expect_error(verify_webhook, secret_hex, header, body, DEFAULT_TOLERANCE_MS)
    assert "malformed" in str(err)


def test_multibyte_utf8_body():
    secret = bytes(32)
    secret_hex = secret.hex()
    body = '{"message":"hello \U0001F389 你好"}'.encode("utf-8")
    ts = now_ts()
    header = f"t={ts},v1={sign(secret, ts, body)}"
    verify_webhook(secret_hex, header, body, DEFAULT_TOLERANCE_MS)


def test_header_constants():
    assert SIGNATURE_HEADER == "X-Crucible-Signature"
    assert TIMESTAMP_HEADER == "X-Crucible-Timestamp"
    assert WEBHOOK_EVENT_ID_HEADER == "X-Webhook-Event-ID"
    assert WEBHOOK_EVENT_TYPE_HEADER == "X-Webhook-Event-Type"
    assert DEFAULT_TOLERANCE_MS == 5 * 60 * 1000
