package config

import (
	"os"
	"strings"
	"testing"
	"time"
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
		{"WorkerRetryMax", c.WorkerRetryMax, 0},
		{"WorkerRetryBackoffMS", c.WorkerRetryBackoffMS, 100},
		{"WorkerBreakerThreshold", c.WorkerBreakerThreshold, 0},
		{"WorkerBreakerCooldownMS", c.WorkerBreakerCooldownMS, 5000},
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

func TestWorkerMaxConnsZeroDefaultsTo64(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "GATEWAY_WORKER_MAX_CONNS", "0")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error for GATEWAY_WORKER_MAX_CONNS=0: %v", err)
	}
	if c.WorkerMaxConns != 64 {
		t.Errorf("WorkerMaxConns = %d, want 64 (silent default for zero/negative)", c.WorkerMaxConns)
	}
}

func TestRetryMaxWithZeroBackoffReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_RETRY_MAX", "3")
	setenv(t, "WORKER_RETRY_BACKOFF_MS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_RETRY_MAX>1 with WORKER_RETRY_BACKOFF_MS=0, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_RETRY_BACKOFF_MS") {
		t.Errorf("error %q does not mention WORKER_RETRY_BACKOFF_MS", err.Error())
	}
}

// TestRetryMaxOneZeroBackoffIsValid documents that WORKER_RETRY_MAX=1 (single-shot
// with no retries) is accepted even when WORKER_RETRY_BACKOFF_MS=0, because backoff
// is never used when only one attempt is made.
func TestRetryMaxOneZeroBackoffIsValid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_RETRY_MAX", "1")
	setenv(t, "WORKER_RETRY_BACKOFF_MS", "0")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error for WORKER_RETRY_MAX=1 with WORKER_RETRY_BACKOFF_MS=0: %v", err)
	}
	if c.WorkerRetryMax != 1 {
		t.Errorf("WorkerRetryMax = %d, want 1", c.WorkerRetryMax)
	}
}

func TestBreakerThresholdTooHighReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_BREAKER_THRESHOLD", "101")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_BREAKER_THRESHOLD=101, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_BREAKER_THRESHOLD") {
		t.Errorf("error %q does not mention WORKER_BREAKER_THRESHOLD", err.Error())
	}
}

func TestBreakerCooldownTooLowReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_BREAKER_THRESHOLD", "5")
	setenv(t, "WORKER_BREAKER_COOLDOWN_MS", "100")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for cooldown < 500ms, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_BREAKER_COOLDOWN_MS") {
		t.Errorf("error %q does not mention WORKER_BREAKER_COOLDOWN_MS", err.Error())
	}
}

// TestBreakerCooldownTooLowZeroThresholdReturnsError verifies that a cooldown
// below 500ms is rejected even when the breaker is disabled (threshold=0),
// preventing a config landmine that only surfaces when threshold is later raised.
func TestBreakerCooldownTooLowZeroThresholdReturnsError(t *testing.T) {
	setRequiredEnv(t)
	// threshold=0 (default, breaker disabled) but cooldown too low.
	setenv(t, "WORKER_BREAKER_COOLDOWN_MS", "100")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_BREAKER_COOLDOWN_MS=100 even with threshold=0, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_BREAKER_COOLDOWN_MS") {
		t.Errorf("error %q does not mention WORKER_BREAKER_COOLDOWN_MS", err.Error())
	}
}

func TestWorkerMaxConnsTooHighReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "GATEWAY_WORKER_MAX_CONNS", "10001")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for GATEWAY_WORKER_MAX_CONNS=10001, got nil")
	}
	if !strings.Contains(err.Error(), "GATEWAY_WORKER_MAX_CONNS") {
		t.Errorf("error %q does not mention GATEWAY_WORKER_MAX_CONNS", err.Error())
	}
}

func TestRetryBackoffTooHighReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_RETRY_MAX", "3")
	setenv(t, "WORKER_RETRY_BACKOFF_MS", "60001")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_RETRY_BACKOFF_MS=60001, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_RETRY_BACKOFF_MS") {
		t.Errorf("error %q does not mention WORKER_RETRY_BACKOFF_MS", err.Error())
	}
}

func TestRetryMaxNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_RETRY_MAX", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_RETRY_MAX=-1, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_RETRY_MAX") {
		t.Errorf("error %q does not mention WORKER_RETRY_MAX", err.Error())
	}
}

func TestWorkerMaxConnsNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "GATEWAY_WORKER_MAX_CONNS", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for GATEWAY_WORKER_MAX_CONNS=-1, got nil")
	}
	if !strings.Contains(err.Error(), "GATEWAY_WORKER_MAX_CONNS") {
		t.Errorf("error %q does not mention GATEWAY_WORKER_MAX_CONNS", err.Error())
	}
}

// TestConfigDurationHelpers verifies that RetryBaseBackoff and BreakerCooldown
// return the configured millisecond values as time.Duration (with the correct
// * time.Millisecond conversion), preventing nanosecond/millisecond unit mismatch
// when constructing resilience.Policy and resilience.BreakerConfig.
func TestConfigDurationHelpers(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_RETRY_MAX", "3")
	setenv(t, "WORKER_RETRY_BACKOFF_MS", "250")
	setenv(t, "WORKER_BREAKER_COOLDOWN_MS", "2000")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got, want := c.RetryBaseBackoff(), 250*time.Millisecond; got != want {
		t.Errorf("RetryBaseBackoff = %v, want %v", got, want)
	}
	if got, want := c.BreakerCooldown(), 2*time.Second; got != want {
		t.Errorf("BreakerCooldown = %v, want %v", got, want)
	}
}
