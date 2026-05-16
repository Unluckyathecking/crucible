-- Crucible v1 schema. Author: Mohammed Ali Bhai, 2026-05-16.
-- Idempotent: safe to re-run during dev reset.

BEGIN;

-- gen_random_uuid() is built-in on Postgres 13+. pgcrypto is a no-op safety belt.
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- ============================================================================
-- Plans: tier matrix. Per-product clones extend this in 0002_seed_plans.sql.
-- ============================================================================
CREATE TABLE IF NOT EXISTS plans (
  id                    TEXT PRIMARY KEY,            -- 'free' | 'pro' | 'business' | ...
  display_name          TEXT NOT NULL,
  stripe_price_id       TEXT UNIQUE,                 -- NULL for free tier
  rate_limit_per_minute INTEGER NOT NULL CHECK (rate_limit_per_minute >= 0),
  monthly_unit_cap      BIGINT CHECK (monthly_unit_cap IS NULL OR monthly_unit_cap >= 0),
  created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- Customers: one row per Stripe customer.
-- ============================================================================
CREATE TABLE IF NOT EXISTS customers (
  id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  email               TEXT UNIQUE NOT NULL,
  stripe_customer_id  TEXT UNIQUE,
  plan_id             TEXT NOT NULL REFERENCES plans(id),
  created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_customers_stripe ON customers(stripe_customer_id);

-- ============================================================================
-- API keys: hashed at rest; prefix indexed for O(1) lookup before constant-time compare.
-- ============================================================================
CREATE TABLE IF NOT EXISTS api_keys (
  id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  customer_id  UUID NOT NULL REFERENCES customers(id) ON DELETE CASCADE,
  prefix       TEXT NOT NULL,                        -- e.g. 'cru_live_a3f9' (first ~12 chars, safe to display)
  hash         BYTEA NOT NULL,                       -- SHA-256(salt || full_key)
  name         TEXT,                                 -- customer-supplied label ("production", "staging")
  revoked_at   TIMESTAMPTZ,
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_used_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS idx_api_keys_active_prefix ON api_keys(prefix) WHERE revoked_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_api_keys_customer     ON api_keys(customer_id);

-- ============================================================================
-- Usage events: one row per Invoke() call. Periodic flusher pushes to Stripe meter_event.
-- ============================================================================
CREATE TABLE IF NOT EXISTS usage_events (
  id                BIGSERIAL PRIMARY KEY,
  customer_id       UUID NOT NULL REFERENCES customers(id),
  api_key_id        UUID NOT NULL REFERENCES api_keys(id),
  operation         TEXT NOT NULL,
  billable_units    BIGINT NOT NULL CHECK (billable_units >= 1),
  request_id        TEXT NOT NULL,
  flushed_to_stripe BOOLEAN NOT NULL DEFAULT FALSE,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_usage_customer_created ON usage_events(customer_id, created_at);
CREATE INDEX IF NOT EXISTS idx_usage_pending_flush    ON usage_events(created_at) WHERE flushed_to_stripe = FALSE;

-- ============================================================================
-- Webhook events: Stripe webhook idempotency by event_id.
-- ============================================================================
CREATE TABLE IF NOT EXISTS webhook_events (
  event_id    TEXT PRIMARY KEY,                      -- Stripe's evt_...
  type        TEXT NOT NULL,
  payload     JSONB NOT NULL,
  received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ============================================================================
-- Audit log: append-only record of sensitive actions.
-- ============================================================================
CREATE TABLE IF NOT EXISTS audit_log (
  id          BIGSERIAL PRIMARY KEY,
  actor_type  TEXT NOT NULL CHECK (actor_type IN ('customer', 'admin', 'system')),
  actor_id    TEXT,
  action      TEXT NOT NULL,                         -- 'api_key.created' | 'plan.changed' | ...
  target_type TEXT,
  target_id   TEXT,
  details     JSONB,
  created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_audit_actor ON audit_log(actor_type, actor_id, created_at DESC);

-- ============================================================================
-- Default plan tiers. Per-product clones override in 0002_seed_plans.sql.
-- ============================================================================
INSERT INTO plans (id, display_name, stripe_price_id, rate_limit_per_minute, monthly_unit_cap)
VALUES
  ('free',     'Free',     NULL, 60,    1000),
  ('pro',      'Pro',      NULL, 600,   100000),
  ('business', 'Business', NULL, 6000,  NULL)
ON CONFLICT (id) DO NOTHING;

COMMIT;
