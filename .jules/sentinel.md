## 2025-05-24 - Unbounded Goroutines and DB connection exhaustion

**Vulnerability:** Denal of Service (DoS) vulnerability via goroutine and database connection exhaustion. In `gateway/internal/auth/store.go`, an unbounded `go func()` was created on every API key lookup to asynchronously update `last_used_at`.

**Learning:** During periods of high traffic, an unbounded goroutine spawning for each request can quickly exhaust system resources (goroutines, memory) and rapidly deplete the database connection pool (`pgxpool`), causing new lookups and critical system paths to block or fail.

**Prevention:** To avoid unbounded goroutine creation on hot paths, implement bounded background workers using a buffered channel. Additionally, use non-blocking channel sends (`select { case channel <- data: default: }`) to fail securely/fast, dropping non-critical updates (like timestamp updates) rather than slowing down or blocking the primary request execution.
