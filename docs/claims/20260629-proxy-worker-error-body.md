# Claim — surface worker non-2xx response body to operators

Directive for the small-claim worker. Grounded in `main` HEAD `7e8fc20`, direction PR #128
item 3, and the REVIEW "Open at v1" debuggability gap.

## Target

`gateway/internal/proxy/client.go` (+ `client_test.go`) in `unluckyathecking/crucible`.

## Problem

When the worker returns a non-2xx, the gateway logs the status code but not the body, so a
misbehaving worker is hard to diagnose. This is operator-side observability — distinct from
the customer-facing error capture in #115/#125.

## Change

On a non-2xx worker response, capture a **size-bounded** slice of the worker's response body
into the structured operator log (and/or a metric label that is bounded — do not use an
unbounded body as a label). The captured body must NEVER appear in the customer-facing
response envelope.

## Expected outcome / acceptance

- `client.go` reads a bounded amount of the worker error body (e.g. capped via
  `io.LimitReader`) into the structured log only.
- `client_test.go` asserts: (a) a non-2xx worker body IS present in the log output, and
  (b) it is ABSENT from the caller-facing envelope returned to the customer.
- `go test -race ./...` green in `gateway/`.

## Constraints

- This is about diagnostic logging of the response **body** — it is NOT a timeout task.
  `http.Client.Timeout` is already set on this client (false-rationale ledger, crucible #36);
  do not add or alter the timeout.
- Stay within `gateway/internal/proxy/**` (+ its test). Do not touch auth, billing, ratelimit,
  quota, or the frozen proto.
- Keep `Record`/proxy path non-blocking; bound the read so a large/hostile worker body can't
  blow up memory or the log line.
