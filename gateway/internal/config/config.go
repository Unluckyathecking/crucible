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
	return &c, nil
}
