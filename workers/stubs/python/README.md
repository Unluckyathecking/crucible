# Python worker stub

A ~50-line stdlib-only reference worker implementing the frozen Crucible HTTP/JSON contract.
Use it as a starting point for any Python-based Crucible worker.

## Contract

| Endpoint | Method | Description |
|---|---|---|
| `/invoke` | `POST` | Handle one invocation; returns `{"payload": {...}, "billable_units": N}` |
| `/healthz` | `GET` | Readiness check; returns `{"status": "ok"}` |

**Invariant:** `billable_units` is always >= 1 on a successful response (enforced by the gateway).

## Request shape

```json
{
  "operation": "echo",
  "payload": {"any": "object"},
  "metadata": {"units": "3"},
  "request_id": "...",
  "customer_id": "...",
  "plan": "..."
}
```

`metadata.units` (optional integer string) controls the returned `billable_units`.
Any non-positive or non-numeric value falls back to 1.

## Run

```sh
python3 main.py          # default port 8081
python3 main.py 9000     # custom port
PORT=9000 python3 main.py
```

## Smoke test

```sh
# health check
curl localhost:8081/healthz

# invoke with explicit units
curl -X POST localhost:8081/invoke \
  -H 'content-type: application/json' \
  -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'
# → {"payload": {"echo": {"x": "hi"}, "operation": "echo"}, "billable_units": 3}
```

## Tests

```sh
# using pytest (no extra dependencies)
python3 -m pytest test_worker.py -v

# or as a plain script
python3 test_worker.py
```

## Differences from the Go stub

The Go stub uses `workers/sdk-go` (graceful shutdown, zerolog). This stub is
deliberately minimal — stdlib only, no graceful drain — so it is easy to read
and adapt without any toolchain beyond CPython 3.8+.
For production use, add signal handling and a proper WSGI/ASGI server.
