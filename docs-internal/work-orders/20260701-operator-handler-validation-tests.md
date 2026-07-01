# worker:claim — DB-free tests for operator admin-API input-validation 400 branches

Seed file for the `worker:claim` PR. The full directive lives in the PR body.

**Target:** `gateway/internal/operator/handlers.go` — validation branches that return `400`
*before* any Store call; tests added to `gateway/internal/operator/store_test.go` (under the
existing "Handler integration tests" section, ~lines 308-491). Do not create `handlers_test.go`.

**Goal:** Cover the operator admin-API request-validation surface with **DB-free** tests
(`operator.NewStore(nil)` — these branches never touch `s.db`):
- `GetCustomerUsageHandler`: invalid UUID (74-78), invalid `start` (83-86), invalid `end`
  (89-92), paired-param `start.IsZero()!=end.IsZero()` (94-97), ordering `!end.After(start)`
  (98-101).
- `ListAuditEventsHandler`: invalid RFC3339 `start`/`end` + ordering — **no test today**.

Assert status `400` + the stable error `code` before the branch would reach a query. Store
stays SELECT-only — do not add mutating paths. Reuse the existing `chi.NewRouter()` +
`operator.Middleware(...)` + `httptest` harness. Parallel-safe with the operator-console UI
job (#138), which is `dashboard/`-only.
