## 2026-05-25 - Prevent MVCC Bloat on Idempotent Upserts
**Learning:** Using `INSERT ... ON CONFLICT DO UPDATE SET x = x` for idempotent lookups is functionally correct but causes Postgres to write a new row version (dead tuple) and WAL entry on every execution, leading to severe MVCC bloat and vacuum overhead on read-heavy paths.
**Action:** Always attempt a pure `SELECT` first for lookups that are expected to exist 99% of the time, falling back to the `INSERT ... ON CONFLICT DO NOTHING` only when the row is missing.
