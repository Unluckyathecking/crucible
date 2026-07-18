# worker:claim brief — document undocumented gateway env knobs in .env.example

## Problem (verified against HEAD d4093f9)
14 `envconfig` knobs declared in `gateway/internal/config/config.go` have **no entry** in
root `.env.example` (verified absent by grep, not merely commented). Operators cannot
discover them from the documented template. Most significant:
- `OTEL_TRACING_ENABLED`, `OTEL_EXPORTER_ENDPOINT`, `OTEL_EXPORTER_INSECURE`,
  `OTEL_SAMPLE_RATIO` (config.go:156-161) — gate an **entire wired tracing subsystem**
  (`internal/tracing/provider.go`, `tracing/middleware.go`) that is undiscoverable today.
- `OPERATOR_TOKEN` (config.go:58) — gates `/v1/admin/*`; an operator can't set it from the template.
- `JOB_TIMEOUT_MS`, `JOB_WORKER_POOL_SIZE`, `JOB_MAX_ATTEMPTS`, `JOB_POLL_INTERVAL_MS`,
  `JOB_REAPER_INTERVAL_MS`, `JOB_RETENTION_DAYS`, `JOB_RETRY_BACKOFF_MS`,
  `DASHBOARD_ORIGIN`, `ERROR_PAYLOAD_CAPTURE`, `ERROR_PAYLOAD_MAX_BYTES`,
  `RESP_CACHE_MAX_TTL_SECONDS`.

`scripts/doctor.sh` only checks salt consistency (doctor.sh:56), not knob completeness;
no test asserts config.go ↔ .env.example parity.

## Directive
Add a documented entry (name, safe default matching config.go's default, one-line comment)
for each of the 14 missing knobs to `.env.example`, grouped logically (tracing, jobs,
operator, misc) to match the file's existing structure. Values must match the defaults in
`config.go` exactly; secrets (OPERATOR_TOKEN) documented as a placeholder, never a real value.

Optional (only if cleanly in scope): a small completeness check in `scripts/doctor.sh` or a
Go test asserting every `envconfig` key in config.go appears in `.env.example`.

## Acceptance
- All 14 named knobs appear in `.env.example` with defaults matching `config.go`.
- No knob documented with a non-default or a real secret value.
- `.env.example` remains parseable as a dotenv template (no syntax breakage).
- No change to `config.go` behaviour or defaults.

## Forbidden
- Do not change any default value or add/remove a config field in config.go.
- Do not put a real OPERATOR_TOKEN or any real secret in the file — placeholder only.
- Do not touch billing/auth/usage/quota or `gateway/proto/**`.
