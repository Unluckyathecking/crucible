# worker:claim — direct unit test for writeJSONError error envelope

**Repo / area:** `crucible` — `gateway/internal/server/routes.go` (`writeJSONError`)
**Type:** missing tests around existing behaviour (test-only)

## Gap (grounded)

`writeJSONError(w, status, code, msg, retryable)`
(`gateway/internal/server/routes.go:195`) builds the gateway's single
client-facing error envelope. It is only exercised **indirectly** through
invoke-handler tests; there is no direct test pinning the envelope's shape,
so a future refactor could silently change the wire contract (field names,
status propagation, content-type) without a failing test.

## Directive

Add a focused test (a new `TestWriteJSONError` in the existing
`gateway/internal/server/routes_test.go`) that calls `writeJSONError`
directly via `httptest.NewRecorder()` and asserts, across two or three
`(status, code, msg, retryable)` combinations:

1. The HTTP status code written equals the `status` argument.
2. `Content-Type` is `application/json`.
3. The decoded body has a top-level `error` object whose fields are exactly
   `code` (string), `message` (string), and `retryable` (bool), with values
   matching the arguments.

## Constraints

- **Test-only.** Do not modify `routes.go` or any non-test source.
- Stay clear of the worker-error branch and observability changes in open
  PR #62 (which adds a metric increment in `routes.go` + `metrics.go`); this
  claim only reads the existing `writeJSONError` helper. No new dependencies.
- Use a uniquely named test/helper to avoid clashes with existing
  `routes_test.go` symbols.

## Verification

`cd crucible && go test -race ./gateway/internal/server/...` is green and the
new test fails if the error-envelope field names, status, or content-type
change.
