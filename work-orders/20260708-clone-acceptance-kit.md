# 10xworker:job — clone-acceptance-kit (runtime acceptance that a cloned tree runs the frozen contract)

```json
{
  "module": "clone-and-adapt-acceptance-kit",
  "scope": [
    "scripts/acceptance-run.sh",
    "gateway/test/acceptance/**",
    "gateway/test/harness/harness.go",
    ".github/workflows/new-tool-smoke.yml",
    "Makefile"
  ],
  "input": "A freshly-cloned tree produced by scripts/new-tool.sh (workers/active symlink -> stubs/go, seeded plans, generated salt/prefix) plus real Postgres + Redis.",
  "output": "A green acceptance run proving the shipped worker satisfies the frozen HTTP/JSON contract AND that a real metered /v1/<op> request flows gateway -> real workers/active worker -> billing end-to-end inside the cloned tree.",
  "acceptance": [
    "scripts/acceptance-run.sh exists and (a) invokes scripts/conformance-run.sh against the worker referenced by workers/active (grep-verifiable: references workers/active, not a hardcoded stub id), and (b) boots the real gateway + real active-worker binary against Postgres/Redis, seeds a plan+key, POSTs a real /v1/<op> request, and asserts HTTP 200 + billable_units>=1 + exactly one usage_events row.",
    "gateway/test/harness/harness.go gains a WorkerURL (external-process worker) option on Options so the harness can target a real worker binary instead of only an in-process http.Handler; the existing WorkerHandler path is unchanged (diff: new field + branch only; NewGatewayTestServer's existing behavior byte-preserved).",
    "A Go test under gateway/test/acceptance/ boots the gateway pointed at the started real worker, seeds plan+key via existing harness CreatePlan/CreateCustomer helpers, drives /v1/<op>, and asserts billable_units>=1 and CountUsageEvents==1 (reuses harness helpers; does NOT re-mock the worker).",
    ".github/workflows/new-tool-smoke.yml gains a step/job that runs scripts/acceptance-run.sh against the demo clone with Postgres+Redis services; fail-fast semantics preserved; workflow green.",
    "Makefile gains an `acceptance` target invoking scripts/acceptance-run.sh.",
    "No file under gateway/internal/** or gateway/proto/** is modified."
  ],
  "forbidden": [
    "No edits to gateway/proto/tool.proto (frozen).",
    "No changes to gateway/internal/{auth,billing,ratelimit,quota,usage,proxy} runtime behavior — acceptance is observe-only.",
    "No new per-product proto fields; `operation` stays an opaque forwarded string.",
    "Do not weaken or replace smoke-new-tool.sh's compile guarantees — extend the chain, do not rewrite it.",
    "Do not re-assert rate-limit/quota/idempotency behaviors already covered in gateway/test/scenarios (no test-saturation duplication).",
    "No new SDK conformance cases here (that surface is owned by the worker-conformance fixtures).",
    "Must not touch billable_units>=1 enforcement, Store.Revoke, key-hash mirroring, PrefixLen, dispatch-before-record, flusher batch_id, or migration idempotency."
  ]
}
```

The steering directive wants a *second real leaf product* to prove the clone-and-adapt promise, but today's pipeline
only guarantees a clone **compiles**: `scripts/smoke-new-tool.sh` runs `go build/vet` + `cargo check` + `py_compile`
and never boots the gateway; `scripts/conformance-run.sh` proves a worker in isolation but is never invoked by the
smoke test nor pointed at `workers/active`; and `gateway/test/scenarios` exercises the full pipeline only against an
in-process `echoWorker` mock, never the real shipped worker and never inside a cloned tree. This kit closes that gap
by composing the already-shipped `conformance-run.sh` and `gateway/test/harness` into a runtime acceptance bar that
runs the real `workers/active` binary against a cloned tree in CI — de-risking product #2 directly and compounding
across every future clone rather than filling saturated unit tests.

---
_Seeded by the cross-repo sprint planner._
