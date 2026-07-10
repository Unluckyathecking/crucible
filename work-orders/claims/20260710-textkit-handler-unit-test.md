# worker:claim — textkit-handler-unit-test

**Repo/area:** crucible — `workers/stubs/textkit/handler/` (the multi-operation reference worker merged
today in #172). The `handler/` dir currently ships only `handler.go` with no adjacent test file; its
branching logic is exercised only indirectly through the gateway end-to-end harness
(`gateway/test/textkit/textkit_test.go`), never in isolation.

**Concrete change:** Add `workers/stubs/textkit/handler/handler_test.go` — table-driven unit tests for:
- `countWords`: variable `billable_units` and the `words` units label across empty / single / multi-word
  / whitespace-heavy inputs.
- `transform`: `upper` / `lower` / `title` modes plus the defensive `BAD_PAYLOAD` error on an unknown
  mode.
- `slugify` / `slug`: leading/trailing/collapsed non-alphanumeric runs, and `titleCase` first-rune
  upper-casing.
- `Handle`: the `UNKNOWN_OPERATION` default branch for an operation the switch does not recognize.

**Expected outcome:** each operation's pure logic and error branch is covered in isolation, so a
regression in the reference worker's string handling or metering is caught at the worker unit level,
not only via the slower gateway harness.

**Constraints the worker must respect:**
- Pure additive test file; do **not** modify `handler.go` or any other runtime source.
- Assert the response struct field values and the returned `billable_units` directly; do not stand up
  an HTTP server (that path is already covered by the harness test).
- Match the actual field names / error codes emitted by `handler.go` (read it; do not invent codes).
- `cd workers/stubs/textkit && go test -race ./...` green.

Parallel-safe / byte-disjoint from the concurrent `route-response-schema` primary (which touches
`gateway/internal/openapi` + `gateway/test/textkit`, not `workers/stubs/textkit`) and from open PRs
#167 (license/EE) and #168 (CI). Touches no invariant, no CI file, no billing/auth path.

---
_Seeded by the cross-repo sprint planner._
