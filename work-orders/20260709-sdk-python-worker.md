# 10xworker:job — sdk-python-worker (complete the polyglot-worker promise for Python)

```json
{
  "module": "sdk-python-worker",
  "scope": [
    "workers/sdk-python/**",
    "workers/conformance/fixture.json",
    ".github/workflows/worker-conformance.yml"
  ],
  "input": "The frozen HTTP/JSON worker contract (POST /invoke + GET /healthz) plus the shared workers/conformance/fixture.json spec.",
  "output": "A stdlib-first Python worker SDK exposing serve()/create_app() + Request/Response/WorkerError, mirroring sdk-go/sdk-rust/sdk-ts, with a fixture-driven in-process conformance runner wired into the worker-conformance CI matrix.",
  "acceptance": [
    "A complete Python worker is one function passed to crucible.serve(port, handler), matching the sdk-go docstring ergonomics (workers/sdk-go/crucible.go:8-21).",
    "SDK normalises billable_units==0 -> 1 before responding (fixture case billable_units_floor; crucible.go:266-268).",
    "Handler errors surface as HTTP 200 {\"error\":{code,message,retryable}} with NO payload/billable_units keys (fixture apierror_envelope; crucible.go:275-281).",
    "Non-POST /invoke returns 405 (fixture non_post_invoke_method_rejected) — the current stub workers/stubs/python/worker.py:50-59 falls through to 404; the SDK must not repeat that.",
    "Empty/invalid body returns a BAD_REQUEST envelope; GET /healthz returns exactly {\"status\":\"ok\"} byte-for-byte.",
    "Optional WORKER_SHARED_SECRET enables inbound X-Worker-Signature HMAC-SHA256 verification byte-identical to crucible.go:147-207 (t=<ts>,v1=<hex>, 5-min window, constant-time compare).",
    "A conformance runner loads workers/conformance/fixture.json (single source of truth) and asserts all cases in-process, mirroring workers/sdk-ts/conformance/conformance.test.js.",
    "A 'python' entry is added to the fixture-conformance matrix in worker-conformance.yml so the fast in-process path exercises the SDK."
  ],
  "forbidden": [
    "No edits to gateway/proto/tool.proto or per-product fields (invariant 1).",
    "Do NOT alter workers/conformance/fixture.json case semantics or add a python known_divergence to excuse a 405/404 miss — the SDK must be conformant.",
    "Do NOT fork or modify sdk-go/sdk-rust/sdk-ts (invariant 9: extend the shared SDK set, don't diverge it).",
    "Do NOT re-derive the HMAC channel-auth scheme; mirror channelsig / crucible.go exactly.",
    "Do NOT refactor unrelated CI jobs (overlaps CI-hygiene draft #168) — only add the python matrix target.",
    "Keep the SDK stdlib-only; add no package dependency (matches the existing Python stub)."
  ]
}
```

Go, Rust, and TypeScript each ship a real worker SDK plus a fixture-driven in-process conformance
runner reading the single-source-of-truth `workers/conformance/fixture.json`; Python is the only
supported language with **no SDK** — just a hand-rolled ~90-line stub (`workers/stubs/python/worker.py`)
tested via the slower external process-spawn path, which already diverges from the frozen contract
(non-POST `/invoke` returns 404 instead of the canonical 405). Shipping `workers/sdk-python` mirroring
the three-times-proven `Serve()` pattern completes the polyglot promise for the most common API-worker
language and compounds as reusable infrastructure: every future Python clone imports one shared,
conformance-locked SDK instead of re-copying divergent boilerplate (the rationale behind invariant 9).

---
_Seeded by the cross-repo sprint planner._
