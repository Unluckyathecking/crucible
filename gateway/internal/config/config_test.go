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
		t.Errorf("WorkerMaxConns = %d, want 64 (zero is silently promoted to the operational default; negative values are rejected by earlier validation)", c.WorkerMaxConns)
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

// TestRetryMaxOneZeroBackoffIsValid verifies WORKER_RETRY_MAX=1 accepts zero backoff (no retries needed).
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

// TestBreakerCooldownZeroWithThresholdReturnsError verifies zero cooldown is rejected when breaker is enabled.
func TestBreakerCooldownZeroWithThresholdReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_BREAKER_THRESHOLD", "5")
	setenv(t, "WORKER_BREAKER_COOLDOWN_MS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_BREAKER_THRESHOLD>0 with WORKER_BREAKER_COOLDOWN_MS=0, got nil")
	}
	if !strings.Contains(err.Error(), "WORKER_BREAKER_COOLDOWN_MS") {
		t.Errorf("error %q does not mention WORKER_BREAKER_COOLDOWN_MS", err.Error())
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

// TestBreakerCooldownTooLowZeroThresholdReturnsError verifies cooldown < 500ms is rejected even with threshold=0.
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

// TestBreakerCooldownTooHighReturnsError verifies cooldown > 300000ms is rejected.
func TestBreakerCooldownTooHighReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_BREAKER_THRESHOLD", "5")
	setenv(t, "WORKER_BREAKER_COOLDOWN_MS", "300001")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WORKER_BREAKER_COOLDOWN_MS=300001, got nil")
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

// --- OTel tracing field tests ---

func TestOtelTracingDisabledByDefault(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OtelTracingEnabled {
		t.Error("OtelTracingEnabled should default to false")
	}
	if c.OtelExporterEndpoint != "" {
		t.Errorf("OtelExporterEndpoint should default to empty, got %q", c.OtelExporterEndpoint)
	}
	if c.OtelSampleRatio != 1.0 {
		t.Errorf("OtelSampleRatio should default to 1.0, got %g", c.OtelSampleRatio)
	}
	if c.OtelExporterInsecure {
		t.Error("OtelExporterInsecure should default to false (TLS on)")
	}
}

func TestOtelTracingEnabledWithEndpointIsValid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	setenv(t, "OTEL_EXPORTER_ENDPOINT", "localhost:4318")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if !c.OtelTracingEnabled {
		t.Error("OtelTracingEnabled = false, want true")
	}
	if c.OtelExporterEndpoint != "localhost:4318" {
		t.Errorf("OtelExporterEndpoint = %q, want localhost:4318", c.OtelExporterEndpoint)
	}
}

// TestOtelTracingEnabledWithoutEndpointReturnsError verifies tracing enabled without endpoint is rejected.
func TestOtelTracingEnabledWithoutEndpointReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	// OTEL_EXPORTER_ENDPOINT intentionally not set.

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for OTEL_TRACING_ENABLED=true without OTEL_EXPORTER_ENDPOINT, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
	}
}

func TestOtelSampleRatioValidValues(t *testing.T) {
	for _, ratio := range []string{"0.0", "0.5", "1.0"} {
		t.Run("ratio="+ratio, func(t *testing.T) {
			setRequiredEnv(t)
			setenv(t, "OTEL_SAMPLE_RATIO", ratio)

			_, err := Load()
			if err != nil {
				t.Errorf("Load: unexpected error for OTEL_SAMPLE_RATIO=%s: %v", ratio, err)
			}
		})
	}
}

func TestOtelSampleRatioNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_SAMPLE_RATIO", "-0.1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for OTEL_SAMPLE_RATIO=-0.1, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_SAMPLE_RATIO") {
		t.Errorf("error %q does not mention OTEL_SAMPLE_RATIO", err.Error())
	}
}

func TestOtelSampleRatioAboveOneReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_SAMPLE_RATIO", "1.1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for OTEL_SAMPLE_RATIO=1.1, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_SAMPLE_RATIO") {
		t.Errorf("error %q does not mention OTEL_SAMPLE_RATIO", err.Error())
	}
}

func TestOtelSampleRatioNaNReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_SAMPLE_RATIO", "NaN")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for OTEL_SAMPLE_RATIO=NaN, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_SAMPLE_RATIO") {
		t.Errorf("error %q does not mention OTEL_SAMPLE_RATIO", err.Error())
	}
}

func TestOtelSampleRatioInfReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_SAMPLE_RATIO", "+Inf")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for OTEL_SAMPLE_RATIO=+Inf, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_SAMPLE_RATIO") {
		t.Errorf("error %q does not mention OTEL_SAMPLE_RATIO", err.Error())
	}
}

func TestOtelExporterInsecureTrue(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_EXPORTER_INSECURE", "true")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.OtelExporterInsecure {
		t.Error("OtelExporterInsecure = false, want true")
	}
}

// TestOtelExporterEndpointWithSchemeReturnsError verifies http:// scheme is rejected (host:port only).
func TestOtelExporterEndpointWithSchemeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	setenv(t, "OTEL_EXPORTER_ENDPOINT", "http://localhost:4318")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for endpoint with http:// scheme, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
	}
}

// TestOtelExporterInsecureInvalidValueReturnsError verifies non-boolean OTEL_EXPORTER_INSECURE is rejected.
func TestOtelExporterInsecureInvalidValueReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_EXPORTER_INSECURE", "not-a-bool")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for OTEL_EXPORTER_INSECURE=not-a-bool, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_INSECURE") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_INSECURE", err.Error())
	}
}

// TestOtelExporterEndpointWithHttpsSchemeReturnsError verifies https:// scheme is also rejected.
func TestOtelExporterEndpointWithHttpsSchemeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	setenv(t, "OTEL_EXPORTER_ENDPOINT", "https://localhost:4318")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for endpoint with https:// scheme, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
	}
}

// TestOtelTracingEnabledWithInsecureEndpoint verifies OTEL_EXPORTER_INSECURE=true is accepted with valid config.
func TestOtelTracingEnabledWithInsecureEndpoint(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	setenv(t, "OTEL_EXPORTER_ENDPOINT", "localhost:4318")
	setenv(t, "OTEL_EXPORTER_INSECURE", "true")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !c.OtelTracingEnabled {
		t.Error("OtelTracingEnabled = false, want true")
	}
	if c.OtelExporterEndpoint != "localhost:4318" {
		t.Errorf("OtelExporterEndpoint = %q, want localhost:4318", c.OtelExporterEndpoint)
	}
	if !c.OtelExporterInsecure {
		t.Error("OtelExporterInsecure = false, want true")
	}
}

// TestOtelExporterEndpointEmptyHostReturnsError verifies that ":4318" (port-only, no host) is rejected.
func TestOtelExporterEndpointEmptyHostReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_EXPORTER_ENDPOINT", ":4318")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for endpoint with empty host (:4318), got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
	}
}

// TestOtelExporterEndpointTooLongReturnsError verifies endpoints > 4096 bytes are rejected.
func TestOtelExporterEndpointTooLongReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_EXPORTER_ENDPOINT", strings.Repeat("x", 4097))

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for endpoint exceeding maximum length, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
	}
}

// TestOtelExporterEndpointWhitespaceWithTracingEnabledReturnsError verifies whitespace-only endpoint is rejected.
func TestOtelExporterEndpointWhitespaceWithTracingEnabledReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	setenv(t, "OTEL_EXPORTER_ENDPOINT", "   ")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for whitespace-only endpoint with tracing enabled, got nil")
	}
	if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
		t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
	}
}

// TestOtelExporterEndpointUppercaseSchemeReturnsError verifies scheme validation is case-insensitive.
func TestOtelExporterEndpointUppercaseSchemeReturnsError(t *testing.T) {
	for _, endpoint := range []string{"HTTP://localhost:4318", "HTTPS://localhost:4318"} {
		t.Run(endpoint, func(t *testing.T) {
			setRequiredEnv(t)
			setenv(t, "OTEL_TRACING_ENABLED", "true")
			setenv(t, "OTEL_EXPORTER_ENDPOINT", endpoint)

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for endpoint %q with uppercase scheme, got nil", endpoint)
			}
			if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
				t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
			}
		})
	}
}

func TestOtelExporterInsecureFalseByDefault(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OtelExporterInsecure {
		t.Error("OtelExporterInsecure = true, want false (default must be TLS-on)")
	}
}

// TestOtelExporterInsecureExplicitFalse verifies OTEL_EXPORTER_INSECURE=false is accepted.
func TestOtelExporterInsecureExplicitFalse(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_EXPORTER_INSECURE", "false")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OtelExporterInsecure {
		t.Error("OtelExporterInsecure = true, want false for explicit false")
	}
}

// TestOtelExporterEndpointTrimSpace verifies that leading/trailing whitespace is stripped before validation.
func TestOtelExporterEndpointTrimSpace(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "OTEL_TRACING_ENABLED", "true")
	setenv(t, "OTEL_EXPORTER_ENDPOINT", " localhost:4318 ")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.OtelExporterEndpoint != "localhost:4318" {
		t.Errorf("OtelExporterEndpoint = %q, want %q after trim", c.OtelExporterEndpoint, "localhost:4318")
	}
}

// TestOtelExporterEndpointSchemeRejectedWhenTracingDisabled verifies scheme validation applies even when tracing is off.
func TestOtelExporterEndpointSchemeRejectedWhenTracingDisabled(t *testing.T) {
	for _, endpoint := range []string{"http://collector:4318", "https://collector:4318"} {
		t.Run(endpoint, func(t *testing.T) {
			setRequiredEnv(t)
			// OTEL_TRACING_ENABLED left at default (false).
			setenv(t, "OTEL_EXPORTER_ENDPOINT", endpoint)

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for scheme endpoint %q when tracing is disabled, got nil", endpoint)
			}
			if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
				t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
			}
		})
	}
}

// TestOtelExporterEndpointMalformedHostPortReturnsError verifies malformed host:port endpoints are rejected.
func TestOtelExporterEndpointMalformedHostPortReturnsError(t *testing.T) {
	for _, endpoint := range []string{"localhost", "host:port:extra"} {
		t.Run(endpoint, func(t *testing.T) {
			setRequiredEnv(t)
			setenv(t, "OTEL_EXPORTER_ENDPOINT", endpoint)

			_, err := Load()
			if err == nil {
				t.Fatalf("expected error for malformed endpoint %q, got nil", endpoint)
			}
			if !strings.Contains(err.Error(), "OTEL_EXPORTER_ENDPOINT") {
				t.Errorf("error %q does not mention OTEL_EXPORTER_ENDPOINT", err.Error())
			}
		})
	}
}

// --- ErrorPayloadCapture field tests ---

// TestErrorPayloadCaptureDefaultOff verifies the feature is disabled by default
// so no clone captures request bodies without an explicit opt-in.
func TestErrorPayloadCaptureDefaultOff(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.ErrorPayloadCapture {
		t.Error("ErrorPayloadCapture = true, want false (must be off by default)")
	}
}

// TestErrorPayloadCaptureMaxBytesTooSmallReturnsError verifies max-bytes < marker length (12) is rejected.
func TestErrorPayloadCaptureMaxBytesTooSmallReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "ERROR_PAYLOAD_CAPTURE", "true")
	setenv(t, "ERROR_PAYLOAD_MAX_BYTES", "11")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for ERROR_PAYLOAD_MAX_BYTES=11 when capture is on, got nil")
	}
	if !strings.Contains(err.Error(), "ERROR_PAYLOAD_MAX_BYTES") {
		t.Errorf("error %q does not mention ERROR_PAYLOAD_MAX_BYTES", err.Error())
	}
}

// TestErrorPayloadCaptureMaxBytesMinimumValid verifies that max-bytes = 12 (marker length) is accepted.
func TestErrorPayloadCaptureMaxBytesMinimumValid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "ERROR_PAYLOAD_CAPTURE", "true")
	setenv(t, "ERROR_PAYLOAD_MAX_BYTES", "12")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error for ERROR_PAYLOAD_MAX_BYTES=12: %v", err)
	}
	if c.ErrorPayloadMaxBytes != 12 {
		t.Errorf("ErrorPayloadMaxBytes = %d, want 12", c.ErrorPayloadMaxBytes)
	}
}

// TestErrorPayloadCaptureMaxBytesTooLargeReturnsError verifies max-bytes > 1 MiB is rejected.
func TestErrorPayloadCaptureMaxBytesTooLargeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "ERROR_PAYLOAD_CAPTURE", "true")
	setenv(t, "ERROR_PAYLOAD_MAX_BYTES", "1048577")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for ERROR_PAYLOAD_MAX_BYTES=1048577 when capture is on, got nil")
	}
	if !strings.Contains(err.Error(), "ERROR_PAYLOAD_MAX_BYTES") {
		t.Errorf("error %q does not mention ERROR_PAYLOAD_MAX_BYTES", err.Error())
	}
}

// TestErrorPayloadCaptureOffIgnoresBytesValidation verifies ERROR_PAYLOAD_MAX_BYTES=0 is accepted when capture is off.
func TestErrorPayloadCaptureOffIgnoresBytesValidation(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "ERROR_PAYLOAD_MAX_BYTES", "0")
	// capture=false (default); MaxBytes=0 must not trigger validation.
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if c.ErrorPayloadCapture {
		t.Error("ErrorPayloadCapture should be false")
	}
	if c.ErrorPayloadMaxBytes != 0 {
		t.Errorf("ErrorPayloadMaxBytes = %d, want 0", c.ErrorPayloadMaxBytes)
	}
}

// --- Worker channel auth field tests ---

// TestWorkerSharedSecretDefaultEmpty verifies WORKER_SHARED_SECRET defaults to empty (signing disabled).
func TestWorkerSharedSecretDefaultEmpty(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WorkerSharedSecret != "" {
		t.Errorf("WorkerSharedSecret = %q, want empty (signing disabled by default)", c.WorkerSharedSecret)
	}
}

// TestWorkerSharedSecretSet verifies WORKER_SHARED_SECRET is read correctly when set.
func TestWorkerSharedSecretSet(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WORKER_SHARED_SECRET", "super-secret-key-for-worker-auth-12345")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WorkerSharedSecret != "super-secret-key-for-worker-auth-12345" {
		t.Errorf("WorkerSharedSecret = %q, want exact value set", c.WorkerSharedSecret)
	}
}

// TestConfigDurationHelpers verifies RetryBaseBackoff and BreakerCooldown apply the ms→Duration conversion.
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

func TestRespCacheMaxTTLSecondsDefault(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.RespCacheMaxTTLSeconds != 3600 {
		t.Errorf("RespCacheMaxTTLSeconds = %d, want 3600", c.RespCacheMaxTTLSeconds)
	}
}

func TestRespCacheMaxTTLSecondsZeroReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "RESP_CACHE_MAX_TTL_SECONDS", "0")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for RESP_CACHE_MAX_TTL_SECONDS=0, got nil")
	}
	if !strings.Contains(err.Error(), "RESP_CACHE_MAX_TTL_SECONDS") {
		t.Errorf("error %q does not mention RESP_CACHE_MAX_TTL_SECONDS", err.Error())
	}
}

func TestRespCacheMaxTTLSecondsNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "RESP_CACHE_MAX_TTL_SECONDS", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for negative RESP_CACHE_MAX_TTL_SECONDS, got nil")
	}
}

func TestRespCacheMaxTTLSecondsTooHighReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "RESP_CACHE_MAX_TTL_SECONDS", "604801")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for RESP_CACHE_MAX_TTL_SECONDS above 7 days, got nil")
	}
}

// TestClampRespCacheTTL covers a valid TTL, a negative value, and an over-max
// value, per the response-result-cache acceptance criteria.
func TestClampRespCacheTTL(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "RESP_CACHE_MAX_TTL_SECONDS", "60")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := c.ClampRespCacheTTL(30), 30*time.Second; got != want {
		t.Errorf("ClampRespCacheTTL(30) = %v, want %v", got, want)
	}
	if got, want := c.ClampRespCacheTTL(-5), time.Duration(0); got != want {
		t.Errorf("ClampRespCacheTTL(-5) = %v, want %v (never cache)", got, want)
	}
	if got, want := c.ClampRespCacheTTL(0), time.Duration(0); got != want {
		t.Errorf("ClampRespCacheTTL(0) = %v, want %v (never cache)", got, want)
	}
	if got, want := c.ClampRespCacheTTL(120), 60*time.Second; got != want {
		t.Errorf("ClampRespCacheTTL(120) = %v, want %v (clamped to max)", got, want)
	}
}

// --- Async job fairness field tests ---

func TestJobFairnessDefaultsUnbounded(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JobMaxInflightPerCustomer != 0 {
		t.Errorf("JobMaxInflightPerCustomer = %d, want 0 (unbounded/disabled)", c.JobMaxInflightPerCustomer)
	}
	if c.JobMaxQueuedPerCustomer != 0 {
		t.Errorf("JobMaxQueuedPerCustomer = %d, want 0 (unbounded/disabled)", c.JobMaxQueuedPerCustomer)
	}
}

func TestJobMaxInflightPerCustomerNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "JOB_MAX_INFLIGHT_PER_CUSTOMER", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for JOB_MAX_INFLIGHT_PER_CUSTOMER=-1, got nil")
	}
	if !strings.Contains(err.Error(), "JOB_MAX_INFLIGHT_PER_CUSTOMER") {
		t.Errorf("error %q does not mention JOB_MAX_INFLIGHT_PER_CUSTOMER", err.Error())
	}
}

func TestJobMaxInflightPerCustomerAboveWorkerPoolSizeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "JOB_WORKER_POOL_SIZE", "4")
	setenv(t, "JOB_MAX_INFLIGHT_PER_CUSTOMER", "5")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for JOB_MAX_INFLIGHT_PER_CUSTOMER > JOB_WORKER_POOL_SIZE, got nil")
	}
	if !strings.Contains(err.Error(), "JOB_MAX_INFLIGHT_PER_CUSTOMER") {
		t.Errorf("error %q does not mention JOB_MAX_INFLIGHT_PER_CUSTOMER", err.Error())
	}
}

func TestJobMaxInflightPerCustomerWithinWorkerPoolSizeIsValid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "JOB_WORKER_POOL_SIZE", "4")
	setenv(t, "JOB_MAX_INFLIGHT_PER_CUSTOMER", "4")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JobMaxInflightPerCustomer != 4 {
		t.Errorf("JobMaxInflightPerCustomer = %d, want 4", c.JobMaxInflightPerCustomer)
	}
}

func TestJobMaxQueuedPerCustomerNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "JOB_MAX_QUEUED_PER_CUSTOMER", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for JOB_MAX_QUEUED_PER_CUSTOMER=-1, got nil")
	}
	if !strings.Contains(err.Error(), "JOB_MAX_QUEUED_PER_CUSTOMER") {
		t.Errorf("error %q does not mention JOB_MAX_QUEUED_PER_CUSTOMER", err.Error())
	}
}

func TestJobMaxQueuedPerCustomerPositiveIsValid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "JOB_MAX_QUEUED_PER_CUSTOMER", "100")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.JobMaxQueuedPerCustomer != 100 {
		t.Errorf("JobMaxQueuedPerCustomer = %d, want 100", c.JobMaxQueuedPerCustomer)
	}
}

func TestWebhookMaxInflightPerCustomerDefaultDisabled(t *testing.T) {
	setRequiredEnv(t)

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WebhookMaxInflightPerCustomer != 0 {
		t.Errorf("WebhookMaxInflightPerCustomer = %d, want 0 (unbounded/disabled)", c.WebhookMaxInflightPerCustomer)
	}
}

func TestWebhookMaxInflightPerCustomerNegativeReturnsError(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER", "-1")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER=-1, got nil")
	}
	if !strings.Contains(err.Error(), "WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER") {
		t.Errorf("error %q does not mention WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER", err.Error())
	}
}

func TestWebhookMaxInflightPerCustomerPositiveIsValid(t *testing.T) {
	setRequiredEnv(t)
	setenv(t, "WEBHOOK_MAX_INFLIGHT_PER_CUSTOMER", "3")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.WebhookMaxInflightPerCustomer != 3 {
		t.Errorf("WebhookMaxInflightPerCustomer = %d, want 3", c.WebhookMaxInflightPerCustomer)
	}
}
