# Crucible worker SDK (Python)

Stdlib-only Python SDK for Crucible workers. Mirrors `workers/sdk-go`, `workers/sdk-rust`,
and `workers/sdk-ts`: a complete worker is one function passed to `crucible.serve()`.

## Contract

| Endpoint | Method | Description |
|---|---|---|
| `/invoke` | `POST` | Handle one invocation; returns `{"payload": {...}, "billable_units": N}` |
| `/healthz` | `GET` | Readiness check; returns `{"status": "ok"}` |

**Invariant:** `billable_units` is always >= 1 on a successful response; the SDK
normalises `0` to `1` before sending (the gateway enforces this at the trust boundary).

## Usage

```python
import crucible


def handler(req: crucible.Request) -> crucible.Response:
    if req.operation == "":
        raise crucible.WorkerError("BAD_REQUEST", "operation is required")
    return crucible.Response(payload={"echo": req.payload}, billable_units=1)


if __name__ == "__main__":
    crucible.serve(8081, handler)
```

## Channel authentication (optional)

Set `WORKER_SHARED_SECRET` to require inbound `/invoke` requests to carry a valid
`X-Worker-Signature: t=<unix-seconds>,v1=<hex-hmac-sha256>` header
(`HMAC-SHA256(secret, timestamp + "." + body)`), byte-identical to the Go/Rust/TS
SDKs and to the gateway's outbound signer. Unset (the default) disables verification.

## Tests

```sh
cd workers/sdk-python
python3 -m unittest conformance.test_fixture_conformance -v
```
