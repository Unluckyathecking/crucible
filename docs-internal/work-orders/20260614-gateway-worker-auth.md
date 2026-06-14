# Work order — gateway↔worker channel authentication (HMAC request signing)

Primary `10xworker:job` decomposition. Downstream 10X workers implement against
this spec. The planner does not write implementation code.

## Spec

```json
{
  "module": "gateway-worker-channel-auth",
  "scope": [
    "gateway/internal/proxy/client.go",
    "gateway/internal/proxy/client_test.go",
    "gateway/internal/config/config.go",
    "gateway/internal/config/config_test.go",
    "gateway/cmd/gateway/main.go",
    "workers/sdk-go/crucible.go",
    "workers/sdk-go/crucible_test.go",
    "workers/sdk-rust/src/server.rs",
    "workers/sdk-ts/src/index.ts",
    "workers/sdk-ts/src/index.test.ts",
    ".env.example",
    "ADAPT.md"
  ],
  "input": "a WORKER_SHARED_SECRET shared between gateway and worker, plus each outbound /invoke request",
  "output": "the gateway signs every /invoke with an HMAC-SHA256 header over a timestamp+body string; each worker SDK verifies it (constant-time compare, bounded timestamp window) and rejects unsigned/forged/stale calls with a non-billable error. Fully no-op (today's behaviour) when the secret is empty.",
  "acceptance": [
    "config.go adds a WORKER_SHARED_SECRET envconfig field (optional, default empty); the empty default leaves all existing behaviour byte-identical, with config_test.go cases for set and unset",
    "proxy/client.go injects an HMAC-SHA256 signature header (timestamp + body scheme mirroring gateway/internal/webhookout/emitter.go) on the /invoke request ONLY when the secret is configured, WITHOUT removing or altering the existing X-Request-ID or traceparent headers; client_test.go asserts header presence, value derivation, and survival of the other headers",
    "each of the three SDKs (Go/Rust/TS) verifies the signature with a constant-time compare (hmac.Equal in Go) and a bounded timestamp window, with dedicated test cases for valid / missing-signature / wrong-secret / tampered-body / stale-timestamp, mirroring webhook_test.go's case matrix",
    "with the secret empty/unset both proxy and SDKs behave exactly as today (a disabled-path test asserts an unsigned call still succeeds) — opt-in, zero-config-safe, matching the OTel/resilience default-off precedent in config.go",
    "verification failure returns a structured error and charges no billable_units; the secret and signature internals are never echoed to the caller",
    "go test -race ./... green in gateway/ and workers/sdk-go/; Rust and TS SDK tests green; .env.example and ADAPT.md document the new var plus the deployment trust note"
  ],
  "forbidden": [
    "no change to gateway/proto/tool.proto (frozen, invariant #1) — channel auth rides HTTP headers, never proto fields",
    "do NOT make signing mandatory-by-default; opt-in only (empty secret == today's behaviour) so existing clones and scripts/smoke-new-tool.sh stay green",
    "do NOT reuse API_KEY_HASH_SALT or any customer API-key material as the worker secret — separate secret, separate purpose",
    "no change to the billable_units>=1 -> 502 trust-boundary check, Store.Revoke, flusher batch_id, Stripe webhook ordering, API-key hash parity (Go/TS), or PrefixLen=24",
    "keep the signature scheme byte-identical across all three SDKs (cross-language parity, in the spirit of invariant #5)",
    "do NOT touch the workers/active symlink, workers/sdk-go/conformance/** (owned by the parallel worker:claim invoke-method-conformance PR), or .kimi-review.yml"
  ]
}
```

## Decomposition (7 subunits)

1. **Config + secret plumbing** — add `WORKER_SHARED_SECRET` to config.go (optional, default empty) with a config_test.go case set+unset. Verify: `go test ./internal/config/...`.
2. **Shared signer helper** — a small HMAC-SHA256 signer over `timestamp.body` mirroring `webhookout/emitter.go`'s scheme and header naming (e.g. `X-Crucible-Worker-Signature` + a timestamp header). One doc-comment line on the WHY. Verify: unit test of derive/format.
3. **Proxy injection** — sign the outbound `/invoke` in `proxy/client.go` only when the secret is set; never disturb X-Request-ID / traceparent. Verify: client_test.go header assertions; existing proxy tests stay green.
4. **sdk-go verification** — verify in `crucible.go::invokeHandler` with `hmac.Equal` + timestamp window; reject with a non-billable structured error. Verify: crucible_test.go case matrix.
5. **sdk-rust verification** — mirror the scheme in `server.rs::invoke_handler`. Verify: crate tests.
6. **sdk-ts verification** — mirror the scheme in `index.ts`. Verify: index.test.ts case matrix.
7. **Docs** — `.env.example` + `ADAPT.md`: document the var, that it is opt-in, and the trust note (the worker MUST NOT be network-reachable outside the gateway when the secret is empty).

## Rationale

Every Crucible clone deploys a worker exposing `/invoke`, which today trusts
any network caller: a `grep` across `gateway/` and `workers/` for worker-auth
headers (`X-Worker`, `WorkerSecret`, `WORKER_SECRET`, `Authorization`/`Bearer`
on the proxy/SDK path) returns nothing — the proxy sends only `Content-Type`,
`X-Request-ID`, and `traceparent`. A worker reachable beyond the gateway's
network boundary can therefore be invoked directly, bypassing auth, rate-limit,
quota **and billing** — the same free-usage escape class the `billable_units`
trust boundary closes, but on the ingress side. A shared-secret signing module
is reusable framework infrastructure: it ships once in the proxy + each SDK and
every present and future clone/worker/language inherits it for free. It is the
symmetric counterpart to the already-shipped outbound-webhook signing
(`webhookout`) and the consumer-SDK verify helper (#122) — the same HMAC
discipline applied to the inbound worker hop. Opt-in and default-off, it is
byte-safe for every existing clone and the smoke test.
