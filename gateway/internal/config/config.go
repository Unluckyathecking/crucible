// Package config reads the gateway's operational contract from the environment
// and validates it. Missing required values fail-fast at startup with a clear error.
package config

import (
	"fmt"
	"math"
	"net"
	"strings"
	"time"

	"github.com/kelseyhightower/envconfig"
)

const (
	minErrorPayloadMaxBytes = 12      // must cover len(payloadTruncationMarker)
	maxErrorPayloadMaxBytes = 1048576 // 1 MiB

	maxRespCacheMaxTTLSeconds = 604800 // 7 days
)

type Config struct {
	// Gateway HTTP
	Port           int   `envconfig:"GATEWAY_PORT"             default:"8080"`
	BodyLimitBytes int64 `envconfig:"GATEWAY_BODY_LIMIT_BYTES" default:"1048576"`

	// Worker
	WorkerURL       string `envconfig:"WORKER_URL"           default:"http://localhost:8081"`
	WorkerTimeoutMS int    `envconfig:"WORKER_TIMEOUT_MS"    default:"10000"`
	WorkerMaxConns  int    `envconfig:"GATEWAY_WORKER_MAX_CONNS" default:"64"`

	// Resilience — retry and circuit-breaker for gateway→worker calls.
	// Defaults are disabled (single-shot, no breaker) to preserve current behaviour.
	WorkerRetryMax          int `envconfig:"WORKER_RETRY_MAX"            default:"0"`
	WorkerRetryBackoffMS    int `envconfig:"WORKER_RETRY_BACKOFF_MS"     default:"100"`
	WorkerBreakerThreshold  int `envconfig:"WORKER_BREAKER_THRESHOLD"    default:"0"`
	WorkerBreakerCooldownMS int `envconfig:"WORKER_BREAKER_COOLDOWN_MS"  default:"5000"`

	// Postgres
	PostgresDSN      string `envconfig:"POSTGRES_DSN"       required:"true"`
	PostgresMaxConns int    `envconfig:"POSTGRES_MAX_CONNS" default:"20"`

	// Redis
	RedisURL string `envconfig:"REDIS_URL" required:"true"`

	// Stripe
	StripeSecretKey     string `envconfig:"STRIPE_SECRET_KEY"     required:"true"`
	StripeWebhookSecret string `envconfig:"STRIPE_WEBHOOK_SECRET" required:"true"`
	StripeMeterName     string `envconfig:"STRIPE_METER_NAME"     default:"crucible_units"`

	// Security
	APIKeyPrefix    string `envconfig:"API_KEY_PREFIX"     default:"cru_"`
	APIKeyHashSalt  string `envconfig:"API_KEY_HASH_SALT"  required:"true"`
	DashboardOrigin string `envconfig:"DASHBOARD_ORIGIN"   default:"http://localhost:3001"`
	// OperatorToken gates the /v1/admin/* read-only subrouter. Empty (default)
	// disables the admin routes entirely; they return 401 if reached.
	// Generate with: openssl rand -hex 32
	OperatorToken string `envconfig:"OPERATOR_TOKEN" default:""`

	// Worker channel authentication — opt-in HMAC-SHA256 /invoke request signing.
	// Empty (the default) disables signing, preserving today's behaviour.
	// When set, the gateway signs every /invoke request; the worker SDK verifies it.
	// Generate with: openssl rand -hex 32
	WorkerSharedSecret string `envconfig:"WORKER_SHARED_SECRET" default:""`

	// Error handling
	ErrorExposure string `envconfig:"WORKER_ERROR_EXPOSURE" default:"sanitized"`

	// Opt-in capture of request bodies on 4xx/5xx; default OFF. Never logged/labeled.
	ErrorPayloadCapture  bool `envconfig:"ERROR_PAYLOAD_CAPTURE"   default:"false"`
	ErrorPayloadMaxBytes int  `envconfig:"ERROR_PAYLOAD_MAX_BYTES" default:"4096"`

	// Response result cache — opt-in, content-addressed cache of successful
	// worker responses (see internal/respcache). A route only caches when it
	// declares a positive CacheTTLSeconds; this is the ceiling every such TTL
	// is clamped to, so a per-product route table can never grant an entry an
	// unreasonably long lifetime.
	RespCacheMaxTTLSeconds int `envconfig:"RESP_CACHE_MAX_TTL_SECONDS" default:"3600"`

	// Async job execution (see internal/jobs) — opt-in per route via
	// routes_table.go's AsyncRoutes. Zero-config-safe: AsyncRoutes defaults
	// empty, so these values are inert until a product clone opts a route in.
	JobWorkerPoolSize int `envconfig:"JOB_WORKER_POOL_SIZE" default:"4"`
	JobPollIntervalMS int `envconfig:"JOB_POLL_INTERVAL_MS" default:"1000"`
	JobTimeoutMS      int `envconfig:"JOB_TIMEOUT_MS"       default:"300000"`
	// JobMaxAttempts bounds retries of a retryable (WORKER_UNREACHABLE /
	// transport) async job failure before it dead-letters to terminal
	// 'failed'; a deterministic worker error or a billable_units<1
	// violation is never retried regardless of this value. Conservative
	// default: matches jobs.ExecutorConfig's own zero-value fallback, so
	// the async path retries transient failures even before this value is
	// wired through to jobs.NewExecutor.
	JobMaxAttempts int `envconfig:"JOB_MAX_ATTEMPTS" default:"3"`
	// JobRetryBackoffMS is the base delay before an async job's first
	// retry; each subsequent retry doubles it, bounded (see
	// jobs.ExecutorConfig.RetryBackoff). Matches jobs.ExecutorConfig's own
	// zero-value fallback.
	JobRetryBackoffMS int `envconfig:"JOB_RETRY_BACKOFF_MS" default:"2000"`
	// JobRetentionDays bounds how long a terminal (succeeded, failed)
	// async_jobs row is kept before jobs.Reaper deletes it. Zero-config-safe:
	// defaults to 0, which makes the reaper inert (never deletes) — matching
	// the stance every other Job knob takes of preserving today's behaviour
	// until a product clone opts in.
	JobRetentionDays int `envconfig:"JOB_RETENTION_DAYS" default:"0"`
	// JobReaperIntervalMS is the delay between jobs.Reaper sweeps.
	JobReaperIntervalMS int `envconfig:"JOB_REAPER_INTERVAL_MS" default:"3600000"`

	// Async job multi-tenant fairness (see internal/jobs.Store.Claim) — opt-in,
	// independent of the AsyncRoutes gate above. Zero-value (the default)
	// disables both knobs and preserves today's exact unbounded global-FIFO
	// claim/admission behaviour byte-for-byte; a product clone opts in by
	// setting either to a positive value.
	//
	// JobMaxInflightPerCustomer bounds how many 'running' rows a single
	// customer may occupy at once across the shared worker pool. 0 (default)
	// disables the per-customer cap — Claim falls back to its original
	// pure-FIFO query.
	JobMaxInflightPerCustomer int `envconfig:"JOB_MAX_INFLIGHT_PER_CUSTOMER" default:"0"`
	// JobMaxQueuedPerCustomer ceilings a single customer's queued+running
	// backlog; enqueueAsync (routes.go) returns 429 JOB_BACKLOG_EXCEEDED once
	// it's reached. 0 (default) disables the ceiling — enqueue admits
	// unconditionally, as today.
	JobMaxQueuedPerCustomer int `envconfig:"JOB_MAX_QUEUED_PER_CUSTOMER" default:"0"`

	// IdempotencyRetentionDays bounds how long an idempotency_keys row is kept
	// before idempotency.Reaper deletes it. Zero-config-safe: defaults to 0,
	// which makes the reaper inert (never deletes). idempotency_keys rows are
	// deleted only lazily by the Store today (on re-query of the same key), so
	// without this knob the table grows without bound at high request volume.
	IdempotencyRetentionDays int `envconfig:"IDEMPOTENCY_RETENTION_DAYS" default:"0"`
	// IdempotencyReaperIntervalMS is the delay between idempotency.Reaper sweeps.
	IdempotencyReaperIntervalMS int `envconfig:"IDEMPOTENCY_REAPER_INTERVAL_MS" default:"3600000"`

	// WebhookDeliveryRetentionDays bounds how long a delivered webhook_deliveries
	// row is kept before webhookout.DeliveryReaper deletes it. Zero-config-safe:
	// defaults to 0, making the reaper inert. dead_letter rows are never deleted
	// by the reaper regardless of this setting — operators replay those via the
	// dead-letter replay console.
	WebhookDeliveryRetentionDays int `envconfig:"WEBHOOK_DELIVERY_RETENTION_DAYS" default:"0"`
	// WebhookDeliveryReaperIntervalMS is the delay between webhookout.DeliveryReaper sweeps.
	WebhookDeliveryReaperIntervalMS int `envconfig:"WEBHOOK_DELIVERY_REAPER_INTERVAL_MS" default:"3600000"`

	// WebhookMaxInflightPerCustomer bounds how many 'delivering' webhook_deliveries
	// rows a single customer may occupy at once across the shared delivery worker
	// (see webhookout.Emitter.claimDue). Mirrors JobMaxInflightPerCustomer's
	// opt-in shape: 0 (default) disables the per-customer cap and preserves
	// today's exact single-query global-FIFO claim behaviour byte-for-byte;
	// a product clone opts in by setting a positive value.
	WebhookMaxInflightPerCustomer int `envconfig:"WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER" default:"0"`

	// WebhookEndpointFailureThreshold is the number of consecutive terminal
	// dead-letters (see webhookout.Emitter.markDeadLetter) after which a
	// webhook endpoint auto-disables (see webhookout/health.go). Zero-config-safe:
	// 0 (default) disables auto-disable entirely, leaving today's forever-retry
	// behaviour byte-identical — a product clone opts in by setting a positive value.
	WebhookEndpointFailureThreshold int `envconfig:"WEBHOOK_ENDPOINT_FAILURE_THRESHOLD" default:"0"`

	// Observability
	LogLevel    string `envconfig:"LOG_LEVEL"    default:"info"`
	MetricsPort int    `envconfig:"METRICS_PORT" default:"9090"`

	// Tracing (OTel) — disabled by default; zero-config dials no exporter.
	OtelTracingEnabled   bool   `envconfig:"OTEL_TRACING_ENABLED"   default:"false"`
	OtelExporterEndpoint string `envconfig:"OTEL_EXPORTER_ENDPOINT" default:""`
	// OtelExporterInsecure disables TLS for the OTLP exporter. Default false (TLS on).
	// Set to true for localhost/sidecar collectors that do not serve TLS.
	OtelExporterInsecure bool    `envconfig:"OTEL_EXPORTER_INSECURE" default:"false"`
	OtelSampleRatio      float64 `envconfig:"OTEL_SAMPLE_RATIO"      default:"1.0"`
}

// Load reads config from the environment and validates it.
func Load() (*Config, error) {
	var c Config
	if err := envconfig.Process("", &c); err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	if len(c.APIKeyHashSalt) < 32 {
		return nil, fmt.Errorf("API_KEY_HASH_SALT must be at least 32 bytes (got %d)", len(c.APIKeyHashSalt))
	}
	if c.OperatorToken != "" && len(c.OperatorToken) < 32 {
		return nil, fmt.Errorf("OPERATOR_TOKEN must be at least 32 bytes when set (got %d); generate with: openssl rand -hex 32", len(c.OperatorToken))
	}
	switch c.ErrorExposure {
	case "sanitized", "full":
	default:
		return nil, fmt.Errorf("WORKER_ERROR_EXPOSURE must be 'sanitized' or 'full' (got %q)", c.ErrorExposure)
	}
	// Negative is a misconfiguration error; zero (omitted/unset) is silently
	// promoted to the operational default. These are intentionally separate checks:
	// negative → reject with an error, zero → apply the default.
	if c.WorkerMaxConns < 0 {
		return nil, fmt.Errorf("GATEWAY_WORKER_MAX_CONNS must be >= 0 (got %d)", c.WorkerMaxConns)
	}
	if c.WorkerMaxConns == 0 {
		c.WorkerMaxConns = 64
	}
	if c.WorkerMaxConns > 10000 {
		return nil, fmt.Errorf("GATEWAY_WORKER_MAX_CONNS must be <= 10000 (got %d)", c.WorkerMaxConns)
	}
	if c.WorkerRetryMax < 0 {
		return nil, fmt.Errorf("WORKER_RETRY_MAX must be >= 0 (got %d)", c.WorkerRetryMax)
	}
	if c.WorkerRetryMax > 10 {
		return nil, fmt.Errorf("WORKER_RETRY_MAX must be <= 10 (got %d)", c.WorkerRetryMax)
	}
	if c.WorkerRetryBackoffMS < 0 {
		return nil, fmt.Errorf("WORKER_RETRY_BACKOFF_MS must be >= 0 (got %d)", c.WorkerRetryBackoffMS)
	}
	if c.WorkerRetryBackoffMS > 60000 {
		return nil, fmt.Errorf("WORKER_RETRY_BACKOFF_MS must be <= 60000 (1 minute) (got %d)", c.WorkerRetryBackoffMS)
	}
	if c.WorkerBreakerThreshold < 0 {
		return nil, fmt.Errorf("WORKER_BREAKER_THRESHOLD must be >= 0 (got %d)", c.WorkerBreakerThreshold)
	}
	if c.WorkerBreakerThreshold > 100 {
		return nil, fmt.Errorf("WORKER_BREAKER_THRESHOLD must be <= 100 (got %d)", c.WorkerBreakerThreshold)
	}
	if c.WorkerBreakerCooldownMS < 0 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be >= 0 (got %d)", c.WorkerBreakerCooldownMS)
	}
	if c.WorkerBreakerCooldownMS > 300000 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be <= 300000 (5 minutes) (got %d)", c.WorkerBreakerCooldownMS)
	}
	// Zero cooldown with threshold enabled would panic resilience.NewBreaker
	// (cooldown=0 makes the breaker immediately re-probe on every Allow after
	// opening, defeating its purpose). Reject it here to get a clear config error
	// instead of a startup panic.
	if c.WorkerBreakerThreshold > 0 && c.WorkerBreakerCooldownMS == 0 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be > 0 when WORKER_BREAKER_THRESHOLD > 0 (got 0)")
	}
	// A non-zero cooldown below 500ms causes rapid open/half-open oscillation that
	// defeats the breaker's purpose. Reject it unconditionally (not just when
	// threshold > 0) so an operator who sets cooldown=100 and threshold=0 today
	// cannot silently create a config landmine that breaks startup the moment
	// they enable the breaker by raising threshold.
	if c.WorkerBreakerCooldownMS > 0 && c.WorkerBreakerCooldownMS < 500 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be >= 500 when non-zero (got %d)", c.WorkerBreakerCooldownMS)
	}
	if c.WorkerTimeoutMS <= 0 {
		return nil, fmt.Errorf("WORKER_TIMEOUT_MS must be > 0 (got %d)", c.WorkerTimeoutMS)
	}
	// With retries enabled a zero backoff hammers the worker without any delay.
	// retry.go defaults BaseBackoff to 100ms when <= 0, but reject it explicitly
	// here so the config is self-consistent: retry + no backoff is a misconfiguration.
	if c.WorkerRetryMax > 1 && c.WorkerRetryBackoffMS == 0 {
		return nil, fmt.Errorf("WORKER_RETRY_BACKOFF_MS must be > 0 when WORKER_RETRY_MAX > 1 (got %d)", c.WorkerRetryBackoffMS)
	}
	// Note: WORKER_BREAKER_THRESHOLD > 0 with WORKER_RETRY_MAX <= 1 is valid but
	// aggressive — every threshold-th single-shot failure opens the breaker with no
	// retry mitigation. Operators should understand this interaction before deploying.
	if c.ErrorPayloadCapture && c.ErrorPayloadMaxBytes < minErrorPayloadMaxBytes {
		return nil, fmt.Errorf("ERROR_PAYLOAD_MAX_BYTES must be >= %d (truncation marker length) when ERROR_PAYLOAD_CAPTURE=true (got %d)", minErrorPayloadMaxBytes, c.ErrorPayloadMaxBytes)
	}
	if c.ErrorPayloadCapture && c.ErrorPayloadMaxBytes > maxErrorPayloadMaxBytes {
		return nil, fmt.Errorf("ERROR_PAYLOAD_MAX_BYTES must be <= %d (1 MiB) when ERROR_PAYLOAD_CAPTURE=true (got %d)", maxErrorPayloadMaxBytes, c.ErrorPayloadMaxBytes)
	}
	if c.RespCacheMaxTTLSeconds <= 0 {
		return nil, fmt.Errorf("RESP_CACHE_MAX_TTL_SECONDS must be > 0 (got %d)", c.RespCacheMaxTTLSeconds)
	}
	if c.RespCacheMaxTTLSeconds > maxRespCacheMaxTTLSeconds {
		return nil, fmt.Errorf("RESP_CACHE_MAX_TTL_SECONDS must be <= %d (7 days) (got %d)", maxRespCacheMaxTTLSeconds, c.RespCacheMaxTTLSeconds)
	}
	if c.JobWorkerPoolSize <= 0 {
		return nil, fmt.Errorf("JOB_WORKER_POOL_SIZE must be > 0 (got %d)", c.JobWorkerPoolSize)
	}
	if c.JobWorkerPoolSize > 256 {
		return nil, fmt.Errorf("JOB_WORKER_POOL_SIZE must be <= 256 (got %d)", c.JobWorkerPoolSize)
	}
	if c.JobPollIntervalMS <= 0 {
		return nil, fmt.Errorf("JOB_POLL_INTERVAL_MS must be > 0 (got %d)", c.JobPollIntervalMS)
	}
	if c.JobTimeoutMS <= 0 {
		return nil, fmt.Errorf("JOB_TIMEOUT_MS must be > 0 (got %d)", c.JobTimeoutMS)
	}
	if c.JobMaxAttempts <= 0 {
		return nil, fmt.Errorf("JOB_MAX_ATTEMPTS must be > 0 (got %d)", c.JobMaxAttempts)
	}
	if c.JobMaxAttempts > 20 {
		return nil, fmt.Errorf("JOB_MAX_ATTEMPTS must be <= 20 (got %d)", c.JobMaxAttempts)
	}
	if c.JobRetryBackoffMS <= 0 {
		return nil, fmt.Errorf("JOB_RETRY_BACKOFF_MS must be > 0 (got %d)", c.JobRetryBackoffMS)
	}
	if c.JobRetryBackoffMS > 60000 {
		return nil, fmt.Errorf("JOB_RETRY_BACKOFF_MS must be <= 60000 (1 minute) (got %d)", c.JobRetryBackoffMS)
	}
	// Unlike the other Job knobs above, zero is a valid, meaningful value
	// here (disables the reaper) rather than a placeholder promoted to a
	// default — only negative is a misconfiguration error.
	if c.JobRetentionDays < 0 {
		return nil, fmt.Errorf("JOB_RETENTION_DAYS must be >= 0 (got %d)", c.JobRetentionDays)
	}
	if c.JobRetentionDays > 3650 {
		return nil, fmt.Errorf("JOB_RETENTION_DAYS must be <= 3650 (10 years) (got %d)", c.JobRetentionDays)
	}
	if c.JobReaperIntervalMS <= 0 {
		return nil, fmt.Errorf("JOB_REAPER_INTERVAL_MS must be > 0 (got %d)", c.JobReaperIntervalMS)
	}
	if c.JobReaperIntervalMS > 86400000 {
		return nil, fmt.Errorf("JOB_REAPER_INTERVAL_MS must be <= 86400000 (24 hours) (got %d)", c.JobReaperIntervalMS)
	}
	// Zero is the valid, meaningful "disabled" value for both fairness knobs
	// (preserves today's unbounded global-FIFO behaviour) — only negative is
	// a misconfiguration error, mirroring JobRetentionDays above.
	if c.JobMaxInflightPerCustomer < 0 {
		return nil, fmt.Errorf("JOB_MAX_INFLIGHT_PER_CUSTOMER must be >= 0 (got %d)", c.JobMaxInflightPerCustomer)
	}
	if c.JobMaxInflightPerCustomer > c.JobWorkerPoolSize {
		return nil, fmt.Errorf("JOB_MAX_INFLIGHT_PER_CUSTOMER must be <= JOB_WORKER_POOL_SIZE (%d) when set (got %d)", c.JobWorkerPoolSize, c.JobMaxInflightPerCustomer)
	}
	if c.JobMaxQueuedPerCustomer < 0 {
		return nil, fmt.Errorf("JOB_MAX_QUEUED_PER_CUSTOMER must be >= 0 (got %d)", c.JobMaxQueuedPerCustomer)
	}
	if c.IdempotencyRetentionDays < 0 {
		return nil, fmt.Errorf("IDEMPOTENCY_RETENTION_DAYS must be >= 0 (got %d)", c.IdempotencyRetentionDays)
	}
	if c.IdempotencyRetentionDays > 3650 {
		return nil, fmt.Errorf("IDEMPOTENCY_RETENTION_DAYS must be <= 3650 (10 years) (got %d)", c.IdempotencyRetentionDays)
	}
	if c.IdempotencyReaperIntervalMS <= 0 {
		return nil, fmt.Errorf("IDEMPOTENCY_REAPER_INTERVAL_MS must be > 0 (got %d)", c.IdempotencyReaperIntervalMS)
	}
	if c.IdempotencyReaperIntervalMS > 86400000 {
		return nil, fmt.Errorf("IDEMPOTENCY_REAPER_INTERVAL_MS must be <= 86400000 (24 hours) (got %d)", c.IdempotencyReaperIntervalMS)
	}
	if c.WebhookDeliveryRetentionDays < 0 {
		return nil, fmt.Errorf("WEBHOOK_DELIVERY_RETENTION_DAYS must be >= 0 (got %d)", c.WebhookDeliveryRetentionDays)
	}
	if c.WebhookDeliveryRetentionDays > 3650 {
		return nil, fmt.Errorf("WEBHOOK_DELIVERY_RETENTION_DAYS must be <= 3650 (10 years) (got %d)", c.WebhookDeliveryRetentionDays)
	}
	if c.WebhookDeliveryReaperIntervalMS <= 0 {
		return nil, fmt.Errorf("WEBHOOK_DELIVERY_REAPER_INTERVAL_MS must be > 0 (got %d)", c.WebhookDeliveryReaperIntervalMS)
	}
	if c.WebhookDeliveryReaperIntervalMS > 86400000 {
		return nil, fmt.Errorf("WEBHOOK_DELIVERY_REAPER_INTERVAL_MS must be <= 86400000 (24 hours) (got %d)", c.WebhookDeliveryReaperIntervalMS)
	}
	// <= 0 (default 0) preserves today's unbounded global-FIFO behaviour
	// (mirrors JobMaxInflightPerCustomer's validation above) — only negative
	// is a misconfiguration error.
	if c.WebhookMaxInflightPerCustomer < 0 {
		return nil, fmt.Errorf("WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER must be >= 0 (got %d)", c.WebhookMaxInflightPerCustomer)
	}
	// <= 0 (default 0) disables auto-disable entirely — only negative is a
	// misconfiguration error, matching WebhookMaxInflightPerCustomer's validation.
	if c.WebhookEndpointFailureThreshold < 0 {
		return nil, fmt.Errorf("WEBHOOK_ENDPOINT_FAILURE_THRESHOLD must be >= 0 (got %d)", c.WebhookEndpointFailureThreshold)
	}
	// --- OTel tracing validation ---
	// NaN fails all comparisons in Go, so it must be checked explicitly — strconv.ParseFloat
	// accepts "NaN" and "Inf" from env vars, both of which would produce undefined sampler behaviour.
	if c.OtelSampleRatio < 0.0 || c.OtelSampleRatio > 1.0 || math.IsNaN(c.OtelSampleRatio) || math.IsInf(c.OtelSampleRatio, 0) {
		return nil, fmt.Errorf("OTEL_SAMPLE_RATIO must be a finite number in [0.0, 1.0] (got %g)", c.OtelSampleRatio)
	}
	// Trim whitespace so " localhost:4318" or "localhost:4318 " (a common copy-paste
	// mistake) is treated equivalently to "localhost:4318". Do this before the empty
	// check so a whitespace-only value is caught by the enabled+empty guard below.
	c.OtelExporterEndpoint = strings.TrimSpace(c.OtelExporterEndpoint)
	// Reject unreasonably long endpoint strings after trim to prevent memory pressure
	// from environment variables that are accidentally set to large values.
	const maxEndpointLen = 4096
	if len(c.OtelExporterEndpoint) > maxEndpointLen {
		return nil, fmt.Errorf("OTEL_EXPORTER_ENDPOINT exceeds maximum length %d (got %d)", maxEndpointLen, len(c.OtelExporterEndpoint))
	}
	// Scheme validation is unconditional: an endpoint with a scheme is always wrong
	// regardless of whether tracing is enabled, preventing latent misconfigurations.
	if c.OtelExporterEndpoint != "" {
		lower := strings.ToLower(c.OtelExporterEndpoint)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return nil, fmt.Errorf("OTEL_EXPORTER_ENDPOINT must be host:port without scheme (got %q)", c.OtelExporterEndpoint)
		}
		host, _, err := net.SplitHostPort(c.OtelExporterEndpoint)
		if err != nil {
			return nil, fmt.Errorf("OTEL_EXPORTER_ENDPOINT must be valid host:port (got %q): %w", c.OtelExporterEndpoint, err)
		}
		if host == "" {
			return nil, fmt.Errorf("OTEL_EXPORTER_ENDPOINT must have a non-empty host (got %q)", c.OtelExporterEndpoint)
		}
	}
	if c.OtelTracingEnabled && c.OtelExporterEndpoint == "" {
		return nil, fmt.Errorf("OTEL_EXPORTER_ENDPOINT must be set when OTEL_TRACING_ENABLED=true")
	}
	return &c, nil
}

// RetryBaseBackoff converts WorkerRetryBackoffMS to time.Duration.
// Use this when constructing a resilience.Policy to avoid the nanosecond/
// millisecond unit mismatch that occurs with a bare time.Duration(int) cast.
func (c *Config) RetryBaseBackoff() time.Duration {
	return time.Duration(c.WorkerRetryBackoffMS) * time.Millisecond
}

// BreakerCooldown converts WorkerBreakerCooldownMS to time.Duration.
// Use this when constructing a resilience.BreakerConfig to avoid unit mismatch.
func (c *Config) BreakerCooldown() time.Duration {
	return time.Duration(c.WorkerBreakerCooldownMS) * time.Millisecond
}

// JobPollInterval converts JobPollIntervalMS to time.Duration.
func (c *Config) JobPollInterval() time.Duration {
	return time.Duration(c.JobPollIntervalMS) * time.Millisecond
}

// JobTimeout converts JobTimeoutMS to time.Duration.
func (c *Config) JobTimeout() time.Duration {
	return time.Duration(c.JobTimeoutMS) * time.Millisecond
}

// JobRetryBackoff converts JobRetryBackoffMS to time.Duration.
func (c *Config) JobRetryBackoff() time.Duration {
	return time.Duration(c.JobRetryBackoffMS) * time.Millisecond
}

// JobRetention converts JobRetentionDays to time.Duration. Zero (the
// default) means retention is disabled — see jobs.Reaper.Run's nil/zero
// no-op check.
func (c *Config) JobRetention() time.Duration {
	return time.Duration(c.JobRetentionDays) * 24 * time.Hour
}

// JobReaperInterval converts JobReaperIntervalMS to time.Duration.
func (c *Config) JobReaperInterval() time.Duration {
	return time.Duration(c.JobReaperIntervalMS) * time.Millisecond
}

// IdempotencyRetention converts IdempotencyRetentionDays to time.Duration.
// Zero (the default) means retention is disabled — see idempotency.Reaper.Run.
func (c *Config) IdempotencyRetention() time.Duration {
	return time.Duration(c.IdempotencyRetentionDays) * 24 * time.Hour
}

// IdempotencyReaperInterval converts IdempotencyReaperIntervalMS to time.Duration.
func (c *Config) IdempotencyReaperInterval() time.Duration {
	return time.Duration(c.IdempotencyReaperIntervalMS) * time.Millisecond
}

// WebhookDeliveryRetention converts WebhookDeliveryRetentionDays to time.Duration.
// Zero (the default) means retention is disabled — see webhookout.DeliveryReaper.Run.
func (c *Config) WebhookDeliveryRetention() time.Duration {
	return time.Duration(c.WebhookDeliveryRetentionDays) * 24 * time.Hour
}

// WebhookDeliveryReaperInterval converts WebhookDeliveryReaperIntervalMS to time.Duration.
func (c *Config) WebhookDeliveryReaperInterval() time.Duration {
	return time.Duration(c.WebhookDeliveryReaperIntervalMS) * time.Millisecond
}

// ClampRespCacheTTL normalizes a route's requested respcache TTL (seconds)
// into a Duration, capped at RespCacheMaxTTLSeconds. A non-positive input
// means "never cache" and returns 0. The route-level TTL is a compile-time
// value from the per-product route table, not untrusted input, so an
// over-max value is silently capped rather than rejected.
func (c *Config) ClampRespCacheTTL(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > c.RespCacheMaxTTLSeconds {
		seconds = c.RespCacheMaxTTLSeconds
	}
	return time.Duration(seconds) * time.Second
}
