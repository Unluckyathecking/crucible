# Claim — respcache canonicalize integer-precision cache-key collision

**Lane:** `worker:claim` · **Seeded:** 2026-07-06

**Target:** `gateway/internal/respcache/cache.go` (+ `gateway/internal/respcache/cache_test.go`). Small, bounded correctness fix on the freshly-landed #156 module.

**Problem (real, deterministic):** `canonicalize` (`cache.go:71-79`) decodes the request payload with `var v interface{}; json.Unmarshal(payload, &v)` and re-marshals with `json.Marshal(v)`. `encoding/json` decodes every JSON number into `float64`, so any integer payload field above 2^53 loses precision on the round-trip. Two semantically **different** requests — e.g. `{"id":9007199254740992}` and `{"id":9007199254740993}` — canonicalize to the same bytes, hence the same `sha256(operation || \x00 || canonical)` key (`cache.go:54-64`). Because respcache is cross-request/cross-customer content-addressed by design, the second caller receives the **first** caller's cached worker response for a different input. The comment at `cache.go:66-70` ("decode-then-reencode is sufficient") is exactly the assumption that fails for large integers.

**Directive:** Replace the `json.Unmarshal` decode in `canonicalize` with a `json.Decoder` over the payload with `dec.UseNumber()` enabled, then re-marshal. Object-key sorting still comes for free from `json.Marshal` of the decoded `map[string]interface{}`; `json.Number` marshals back as its exact literal digits, so large integers (and high-precision decimals) survive verbatim. Do not otherwise change the key derivation or the `\x00` separator.

**Acceptance:**
- A table test in `cache_test.go` asserts `Key(op, a) != Key(op, b)` for `a={"id":9007199254740992}` and `b={"id":9007199254740993}` (this test FAILS on current main: both collapse to `9.007199254740992e+15`).
- A test asserts canonicalization is still order-insensitive: `{"a":1,"b":2}` and `{"b":2,"a":1}` produce the SAME key.
- `go test -race ./gateway/internal/respcache/` is green.

**Constraints:** Touch only `respcache/cache.go` + its test. Do not change the cache-key input set (operation + payload only; never include the API key/customer id/secret — invariant preserved). No new dependency beyond stdlib `encoding/json`/`bytes`. Parallel-safe / byte-disjoint from the `selferrors-read-api` primary and the `respcache-metrics` claim this cycle (different files).

---
_Seeded by the cross-repo sprint planner._
