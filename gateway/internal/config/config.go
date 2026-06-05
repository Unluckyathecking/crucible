// Package config reads the gateway's operational contract from the environment
// and validates it. Missing required values fail-fast at startup with a clear error.
package config

import (
	"fmt"

	"github.com/kelseyhightower/envconfig"
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

	// Error handling
	ErrorExposure string `envconfig:"WORKER_ERROR_EXPOSURE" default:"sanitized"`

	// Observability
	LogLevel    string `envconfig:"LOG_LEVEL"    default:"info"`
	MetricsPort int    `envconfig:"METRICS_PORT" default:"9090"`
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
	switch c.ErrorExposure {
	case "sanitized", "full":
	default:
		return nil, fmt.Errorf("WORKER_ERROR_EXPOSURE must be 'sanitized' or 'full' (got %q)", c.ErrorExposure)
	}
	if c.WorkerTimeoutMS <= 0 {
		return nil, fmt.Errorf("WORKER_TIMEOUT_MS must be > 0 (got %d)", c.WorkerTimeoutMS)
	}
	if c.WorkerMaxConns < 0 {
		return nil, fmt.Errorf("GATEWAY_WORKER_MAX_CONNS must be >= 0 (got %d)", c.WorkerMaxConns)
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
	if c.WorkerBreakerThreshold > 1000 {
		return nil, fmt.Errorf("WORKER_BREAKER_THRESHOLD must be <= 1000 (got %d)", c.WorkerBreakerThreshold)
	}
	if c.WorkerBreakerCooldownMS < 0 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be >= 0 (got %d)", c.WorkerBreakerCooldownMS)
	}
	if c.WorkerBreakerCooldownMS > 300000 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be <= 300000 (5 minutes) (got %d)", c.WorkerBreakerCooldownMS)
	}
	if c.WorkerBreakerThreshold > 0 && c.WorkerBreakerCooldownMS == 0 {
		return nil, fmt.Errorf("WORKER_BREAKER_COOLDOWN_MS must be > 0 when WORKER_BREAKER_THRESHOLD > 0 (got %d)", c.WorkerBreakerCooldownMS)
	}
	// With retries enabled a zero backoff hammers the worker without any delay.
	// retry.go defaults BaseBackoff to 100ms when <= 0, but reject it explicitly
	// here so the config is self-consistent: retry + no backoff is a misconfiguration.
	if c.WorkerRetryMax > 1 && c.WorkerRetryBackoffMS == 0 {
		return nil, fmt.Errorf("WORKER_RETRY_BACKOFF_MS must be > 0 when WORKER_RETRY_MAX > 1 (got %d)", c.WorkerRetryBackoffMS)
	}
	return &c, nil
}
