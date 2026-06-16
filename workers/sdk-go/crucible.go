// Package crucible is the Go SDK for Crucible workers.
//
// A worker is just an HTTP server with two endpoints:
//
//	POST /invoke   — handles one Invoke() request, returns the result + billable_units
//	GET  /healthz  — returns 200 OK when ready
//
// This SDK provides the boilerplate so a complete worker is one function:
//
//	package main
//
//	import (
//	    "context"
//	    crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
//	)
//
//	func main() {
//	    crucible.Serve(8081, func(ctx context.Context, in crucible.Request) (crucible.Response, error) {
//	        return crucible.Response{Payload: map[string]string{"hello": "world"}}, nil
//	    })
//	}
package crucible

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Request mirrors the InvokeRequest proto for handlers that don't depend on generated proto types.
type Request struct {
	RequestID  string            `json:"request_id"`
	CustomerID string            `json:"customer_id"`
	Operation  string            `json:"operation"`
	Payload    json.RawMessage   `json:"payload"`
	Plan       string            `json:"plan"`
	Metadata   map[string]string `json:"metadata"`
}

// Response is what a handler returns on success. BillableUnits defaults to 1 if zero.
type Response struct {
	Payload       any    `json:"payload"`
	BillableUnits uint64 `json:"billable_units"`
	UnitsLabel    string `json:"units_label,omitempty"`
}

// Error is a structured error a handler can return to surface a stable code to the customer.
// Workers may also return a plain `error` — the SDK wraps it as a generic INTERNAL error
// (the real cause is logged but never surfaced).
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// HandlerFunc is the worker's single entry point.
type HandlerFunc func(ctx context.Context, in Request) (Response, error)

// HandlerConfig holds optional configuration for the worker HTTP handler.
type HandlerConfig struct {
	// SharedSecret is the HMAC-SHA256 key for inbound /invoke request verification.
	// Empty disables verification (today's behaviour). When Handler() is called
	// directly, WORKER_SHARED_SECRET from the environment is used automatically.
	SharedSecret string
	// metrics is wired in by Serve when WORKER_METRICS_PORT is set.
	// nil disables metrics recording — preserving today's behaviour exactly.
	metrics *workerMetrics
}

// Handler returns an http.Handler that serves /healthz and /invoke for h.
// When WORKER_SHARED_SECRET is set in the environment, inbound /invoke requests
// are verified against an HMAC-SHA256 signature before the handler is called.
// The returned handler can be used with httptest.NewServer or http.Server directly.
// Returns an error only if h is nil.
func Handler(h HandlerFunc) (http.Handler, error) {
	return HandlerWithConfig(h, HandlerConfig{SharedSecret: os.Getenv("WORKER_SHARED_SECRET")})
}

// HandlerWithConfig returns an http.Handler with explicit configuration.
// Use this in tests or when configuring the secret programmatically rather than
// via the WORKER_SHARED_SECRET environment variable.
func HandlerWithConfig(h HandlerFunc, cfg HandlerConfig) (http.Handler, error) {
	if h == nil {
		return nil, errors.New("crucible.Handler: nil HandlerFunc")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/invoke", invokeHandler(h, cfg.SharedSecret, cfg.metrics))
	return mux, nil
}

// initMetrics reads WORKER_METRICS_PORT, starts a listener on that port if set, and
// returns the metrics handle and the running metrics HTTP server. Both are nil when the
// port is unset or invalid — keeping metrics off by default so existing clones and smoke
// tests continue unchanged.
func initMetrics() (*workerMetrics, *http.Server) {
	portStr := os.Getenv("WORKER_METRICS_PORT")
	if portStr == "" {
		return nil, nil
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 || p > 65535 {
		log.Warn().Str("WORKER_METRICS_PORT", portStr).Msg("invalid WORKER_METRICS_PORT: must be 1-65535; metrics disabled")
		return nil, nil
	}
	m := newWorkerMetrics()
	mSrv, err := startMetricsListener(p, m)
	if err != nil {
		log.Warn().Err(err).Int("metrics_port", p).Msg("WORKER_METRICS_PORT bind failed; metrics disabled")
		return nil, nil
	}
	return m, mSrv
}

// Serve runs the worker HTTP server on the given port and blocks until SIGINT/SIGTERM,
// then drains in-flight requests for up to 10s. When WORKER_METRICS_PORT is set, the
// metrics server is also shut down gracefully before Serve returns.
func Serve(port int, h HandlerFunc) error {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	m, mSrv := initMetrics()
	handler, err := HandlerWithConfig(h, HandlerConfig{
		SharedSecret: os.Getenv("WORKER_SHARED_SECRET"),
		metrics:      m,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		log.Info().Int("port", port).Msg("worker listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("worker server failed")
		}
	}()

	<-shutdown
	log.Info().Msg("worker shutting down")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if mSrv != nil {
		_ = mSrv.Shutdown(ctx)
	}
	return srv.Shutdown(ctx)
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// workerSigHeader is the header carrying the inbound channel-auth signature.
// Format: t=<unix-seconds>,v1=<hex-sha256-hmac>
// Signing payload: HMAC-SHA256(secret, timestamp + "." + body) — byte-identical
// to the outbound webhook scheme so the same scheme is used in both directions.
const workerSigHeader = "X-Worker-Signature"

// workerSigWindow is the maximum age (or future skew) allowed for a signed request.
// Mirrors the Stripe webhook replay window used in billing/webhook.go.
const workerSigWindow = 5 * time.Minute

// verifyWorkerSig verifies the X-Worker-Signature header against body using secret.
// It parses the "t=<ts>,v1=<hex>" format, checks the timestamp window, and does a
// constant-time HMAC comparison. Returns a non-nil error on any verification failure.
// The error text is never forwarded to the caller; only UNAUTHORIZED is surfaced.
func verifyWorkerSig(header string, body []byte, secret []byte) error {
	if header == "" {
		return errors.New("missing signature header")
	}

	var tsStr, sigHex string
	for _, part := range strings.Split(header, ",") {
		switch {
		case strings.HasPrefix(part, "t="):
			tsStr = part[2:]
		case strings.HasPrefix(part, "v1="):
			sigHex = part[3:]
		}
	}
	if tsStr == "" || sigHex == "" {
		return errors.New("malformed signature header")
	}

	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("invalid timestamp in signature header")
	}

	now := time.Now().Unix()
	diff := now - ts
	if diff < 0 {
		diff = -diff
	}
	if diff > int64(workerSigWindow.Seconds()) {
		return errors.New("stale timestamp in signature header")
	}

	provided, err := hex.DecodeString(sigHex)
	if err != nil || len(provided) != sha256.Size {
		return errors.New("invalid signature value")
	}

	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(tsStr))
	_, _ = mac.Write([]byte("."))
	_, _ = mac.Write(body)

	if !hmac.Equal(provided, mac.Sum(nil)) {
		return errors.New("signature mismatch")
	}
	return nil
}

func invokeHandler(h HandlerFunc, secret string, m *workerMetrics) http.HandlerFunc {
	var secretBytes []byte
	if secret != "" {
		secretBytes = []byte(secret)
	}

	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Read raw body first so the signature can be verified before JSON decode.
		// http.MaxBytesReader limits to 10 MiB and returns an error on overrun.
		rawBody, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 10<<20))
		if err != nil {
			writeStructuredError(w, &Error{Code: "BAD_REQUEST", Message: "invalid request body"})
			return
		}

		// Verify the HMAC-SHA256 channel-auth signature when configured.
		// Empty secretBytes → verification disabled → today's behaviour preserved.
		if len(secretBytes) > 0 {
			if err := verifyWorkerSig(r.Header.Get(workerSigHeader), rawBody, secretBytes); err != nil {
				// Surface only a stable code; the signature detail is never echoed.
				writeStructuredError(w, &Error{Code: "UNAUTHORIZED", Message: "invalid request signature"})
				return
			}
		}

		var req Request
		if err := json.Unmarshal(rawBody, &req); err != nil {
			writeStructuredError(w, &Error{Code: "BAD_REQUEST", Message: "invalid request body"})
			return
		}

		// Metric tracking starts after successful decode — operation is now a bounded
		// product-defined string, never a raw URL path or per-request identifier.
		start := time.Now()
		outcome := "ok"
		defer func() {
			if m != nil {
				m.observe(req.Operation, outcome, time.Since(start))
			}
		}()

		resp, err := h(r.Context(), req)
		if err != nil {
			outcome = "error"
			var serr *Error
			if errors.As(err, &serr) {
				log.Info().
					Str("request_id", req.RequestID).
					Str("operation", req.Operation).
					Str("code", serr.Code).
					Msg("handler returned structured error")
				writeStructuredError(w, serr)
				return
			}

			log.Error().
				Str("request_id", req.RequestID).
				Str("operation", req.Operation).
				Err(err).Msg("handler failed")
			writeStructuredError(w, &Error{Code: "INTERNAL", Message: "internal error", Retryable: true})
			return
		}

		if resp.BillableUnits == 0 {
			resp.BillableUnits = 1
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	}
}

// writeStructuredError returns HTTP 200 with an `error` envelope. The gateway distinguishes
// success vs error by the response shape, not the HTTP status — this matches the proto's
// `oneof result { payload | error }` semantics.
func writeStructuredError(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"error": e})
}
