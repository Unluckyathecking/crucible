# worker:claim — conformance test that worker /invoke rejects non-POST with 405

**Repo / area:** `crucible` — `workers/sdk-go/conformance/` (the importable
in-process worker-conformance harness added in #119)
**Type:** missing tests around existing behaviour (test-only)

## Gap (grounded)

All three worker SDKs already reject non-POST requests to `/invoke`:
- sdk-go: `crucible.go:126-127` returns `http.StatusMethodNotAllowed` for
  `r.Method != http.MethodPost`.
- sdk-rust: `server.rs` routes `/invoke` via axum `post(invoke_handler)`
  (framework-enforced 405).
- sdk-ts: `index.ts` guards `req.method !== 'POST'`.

But neither the sdk-go conformance harness (`workers/sdk-go/conformance/`) nor
the language-agnostic conformance suite asserts method rejection — a refactor
could silently drop the 405 guard (e.g. start serving GET) with no failing
test. This is a real, load-bearing transport-contract behaviour with no
regression guard, not a trivial wrapper.

## Directive

Add a conformance assertion (a new case in
`workers/sdk-go/conformance/harness_test.go`, plus a small helper in
`harness.go` if the existing harness has no method-level probe) that, against
the in-process worker, asserts:

1. `GET /invoke` returns HTTP 405.
2. `PUT /invoke` returns HTTP 405.
3. `GET /healthz` still returns HTTP 200 (health probe unaffected).

## Constraints

- **Test/harness-only.** Do not change any SDK runtime behaviour; in
  particular do not edit `crucible.go`'s request handling.
- **Stay clear of the parallel `10xworker:job` gateway-worker-channel-auth
  PR**, which owns `workers/sdk-go/crucible.go`, `sdk-rust/src/server.rs`,
  `sdk-ts/src/index.ts`. This claim only adds a method-rejection assertion to
  the conformance suite — it adds no request signing and touches none of those
  runtime files.
- No new dependencies. Use a uniquely named test/helper to avoid clashing with
  existing `harness_test.go` symbols.

## Verification

`cd crucible/workers/sdk-go && go test -race ./conformance/...` is green, and
the new assertion fails if any SDK starts accepting non-POST on `/invoke`.
