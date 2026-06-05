package config

import (
	"os"
	"strings"
	"testing"
)

func setenv(t *testing.T, key, value string) {
	t.Helper()
	if err := os.Setenv(key, value); err != nil {
		t.Fatalf("setenv %s: %v", key, err)
	}
	t.Cleanup(func() { os.Unsetenv(key) })
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	setenv(t, "POSTGRES_DSN", "postgres://user:pass@localhost:5432/db")
	setenv(t, "REDIS_URL", "redis://localhost:6379")
	setenv(t, "STRIPE_SECRET_KEY", "sk_test_1234")
	setenv(t, "STRIPE_WEBHOOK_SECRET", "whsec_abcd")
	setenv(t, "API_KEY_HASH_SALT", "thirty-two-bytes-of-salt-padding-aaaa")
}

func TestLoadWithAllRequiredSet(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}

	if c.Port != 8080 {
		t.Errorf("Port = %d, want 8080", c.Port)
	}
	if c.PostgresDSN != "postgres://user:pass@localhost:5432/db" {
		t.Errorf("PostgresDSN = %q, want %q", c.PostgresDSN, "postgres://user:pass@localhost:5432/db")
	}
	if c.RedisURL != "redis://localhost:6379" {
		t.Errorf("RedisURL = %q, want %q", c.RedisURL, "redis://localhost:6379")
	}
	if c.APIKeyPrefix != "cru_" {
		t.Errorf("APIKeyPrefix = %q, want cru_", c.APIKeyPrefix)
	}
}

func TestLoadMissingRequiredReturnsError(t *testing.T) {
	_, err := Load()
	if err == nil {
		t.Fatal("expected error for missing required env vars, got nil")
	}
}

func TestLoadSaltTooShortReturnsError(t *testing.T) {
	setRequiredEnv(t)
	saltValue := "short"
	setenv(t, "API_KEY_HASH_SALT", saltValue)

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for short salt, got nil")
	}
	if !strings.Contains(err.Error(), "API_KEY_HASH_SALT") {
		t.Errorf("error %q does not mention API_KEY_HASH_SALT", err.Error())
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	defaults := []struct {
		name string
		got  interface{}
		want interface{}
	}{
		{"Port", c.Port, 8080},
		{"BodyLimitBytes", c.BodyLimitBytes, int64(1048576)},
		{"WorkerURL", c.WorkerURL, "http://localhost:8081"},
		{"WorkerTimeoutMS", c.WorkerTimeoutMS, 10000},
		{"PostgresMaxConns", c.PostgresMaxConns, 20},
		{"StripeMeterName", c.StripeMeterName, "crucible_units"},
		{"APIKeyPrefix", c.APIKeyPrefix, "cru_"},
		{"ErrorExposure", c.ErrorExposure, "sanitized"},
		{"LogLevel", c.LogLevel, "info"},
		{"MetricsPort", c.MetricsPort, 9090},
	}

	for _, d := range defaults {
		t.Run(d.name, func(t *testing.T) {
			if d.got != d.want {
				t.Errorf("%s = %v, want %v", d.name, d.got, d.want)
			}
		})
	}
}

func TestErrorExposureDefault(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ErrorExposure != "sanitized" {
		t.Errorf("ErrorExposure = %q, want 'sanitized'", c.ErrorExposure)
	}
}

func TestErrorExposureFull(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_ERROR_EXPOSURE", "full")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ErrorExposure != "full" {
		t.Errorf("ErrorExposure = %q, want 'full'", c.ErrorExposure)
	}
}

func TestErrorExposureInvalid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_ERROR_EXPOSURE", "detailed")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid WORKER_ERROR_EXPOSURE, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_ERROR_EXPOSURE") {
		t.Errorf("error %q does not mention WORKER_ERROR_EXPOSURE", err.Error())
	}
}

func TestLoadCustomPort(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "GATEWAY_PORT", "3000")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Port != 3000 {
		t.Errorf("Port = %d, want 3000", c.Port)
	}
}

func TestWorkerTimeoutMSZeroReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_TIMEOUT_MS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_TIMEOUT_MS=0, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_TIMEOUT_MS") {
		t.Errorf("error %q does not mention WORKER_TIMEOUT_MS", err.Error())
	}
}

func TestBreakerCooldownTooLowReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_BREAKER_THRESHOLD", "5")
	setenv(t, "WORKER_BREAKER_COOLDOWN_MS", "100")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for cooldown < 500ms with threshold > 0, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_BREAKER_COOLDOWN_MS") {
		t.Errorf("error %q does not mention WORKER_BREAKER_COOLDOWN_MS", err.Error())
	}
}
