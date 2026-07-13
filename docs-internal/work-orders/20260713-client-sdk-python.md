# 10xworker:job — client-sdk-python

Complete the polyglot **consumer** client trio. The generator emits only Go and
TypeScript; there is no generated Python consumer client (`find clients -iname '*.py'`
is empty; `scripts/gen-clients.sh` writes to exactly `GO_DIR=clients/go` and
`TS_DIR=clients/typescript`, and its final banner prints only `Go:`/`TypeScript:`).
Python 3 is the generator's *host* language but is not an emit target. Consequently the
entire endpoint surface — including the async-job surface (`ListJobs`/`GetJob`),
`ListUsageEvents` (#181), and the webhook-endpoint CRUD — is absent in Python, while
both `clients/go/client.go` and `clients/typescript/src/client.ts` already carry them.

This extends the existing extensible generator so every future endpoint and every clone
auto-emits a third language, mirroring the already-shipped polyglot **worker** SDK quartet
(`workers/sdk-{go,rust,ts,python}`). It is a compounding module, not a one-off ticket, and
sits entirely outside the async-framework PRs (#175–#182) and the two owner-decision zones
(#167 open-core/ee, #168 CI-hygiene / setup-go composite).

```json
{
  "module": "client-sdk-python",
  "scope": [
    "scripts/gen-clients.sh",
    "clients/python/**",
    ".github/workflows/client-sdk-drift.yml",
    "docs/specs/openapi-client-sdk-gen.md"
  ],
  "input": "The committed clients/openapi.json snapshot (14 endpoints) plus the existing Go/TS emit logic in scripts/gen-clients.sh",
  "output": "A generated, drift-guarded, pip-installable Python consumer client at clients/python/ covering every endpoint (incl. list_jobs/get_job/list_usage_events) with a typed client, typed ApiError, and a hand-written webhook-verify helper byte-compatible with the Go/TS one",
  "acceptance": [
    "scripts/gen-clients.sh gains a Python emitter block writing clients/python/crucible/client.py and errors.py; the completion banner prints a 'Python:' line alongside Go/TS (diff shows a PY_DIR target added)",
    "clients/python/crucible/client.py exposes a typed method for all 14 clients/openapi.json operations, including list_jobs, get_job, and list_usage_events (grep the generated file for each)",
    "Running `bash scripts/gen-clients.sh` twice produces byte-identical output — `git diff --exit-code clients/python` is clean on the second run (idempotency, matching the existing Go/TS guarantee)",
    "clients/python contains a hand-written webhook.py implementing verify_webhook using hmac.compare_digest (constant-time) with the same signature-header parse, timestamp-tolerance window, and X-Crucible-* headers as clients/go/webhook.go; tests assert a valid signature passes and a tampered body / expired timestamp / wrong secret each fail",
    "clients/python includes pyproject.toml (stdlib-only runtime deps, mirroring the zero-external-dep stance of the Go/TS clients) and passing pytest tests exercising the generated client against a local http stub (success + typed ApiError envelope {error:{code,message,retryable}})",
    ".github/workflows/client-sdk-drift.yml adds clients/python/** to the push and pull_request paths and a Python install+pytest step (setup-python); the existing `git diff --exit-code clients/` drift gate now also covers the Python output",
    "docs/specs/openapi-client-sdk-gen.md 'What was built' table gains a Python client row; no other doc claim is altered",
    "The generated Go/TS output is byte-unchanged — the diff touches no clients/go/** or clients/typescript/** file"
  ],
  "forbidden": [
    "Do NOT change gateway/proto/tool.proto (frozen contract — invariant #1)",
    "Do NOT change any gateway/internal/** code, billing/usage/flusher/webhook logic, or migrations — this is client-side only; the served spec is unchanged",
    "Do NOT edit clients/openapi.json (it is the gateway's spec-dump output; regenerating it is not this module's job)",
    "No signature or behavior change to clients/go/** or clients/typescript/** — generated Go/TS must remain byte-identical",
    "Do NOT touch .github/workflows/kimi-review.yml or introduce/modify a composite setup-go action (overlaps #168's zone); only extend client-sdk-drift.yml, additively",
    "Do NOT touch any ee/ path or add licensing/EE gating (overlaps #167)",
    "webhook.py must be hand-maintained and excluded from the generator's write scope, exactly as webhook.go/webhook.ts are — the generator must not overwrite it",
    "Python webhook-verify semantics MUST stay byte-identical to the Go/TS VerifyWebhook (same HMAC-SHA256 construction, tolerance window, constant-time compare) — a cross-language mirror like the api-key hash mirror; do not diverge the algorithm in one language"
  ]
}
```

**Rationale.** The gap is grep-proven (no `clients/*.py`; the generator targets only
`GO_DIR`/`TS_DIR`) and independent of the async-framework phase (#175–#182 are gateway-side
+ spec/codegen for the existing two languages). Extending the generator to emit a third
consumer language is a reusable, compounding increment — every future endpoint and every
clone gains Python parity for free — and completes the consumer-side mirror of the shipped
polyglot worker-SDK quartet. Scope is ~1.6–1.9k LOC, single phase, well under 10k, and
disjoint from both owner-decision drafts (#167 ee/, #168 setup-go/kimi).
