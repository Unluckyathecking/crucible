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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
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

// Serve runs the worker HTTP server on the given port and blocks until SIGINT/SIGTERM,
// then drains in-flight requests for up to 10s.
func Serve(port int, h HandlerFunc) error {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthHandler)
	mux.HandleFunc("/invoke", invokeHandler(h))

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", port),
		Handler:           mux,
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
	return srv.Shutdown(ctx)
}

func healthHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func invokeHandler(h HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req Request
		// 10 MiB cap; gateway enforces a smaller per-route limit upstream.
		body := http.MaxBytesReader(w, r.Body, 10<<20)
		if err := json.NewDecoder(body).Decode(&req); err != nil {
			writeStructuredError(w, &Error{Code: "BAD_REQUEST", Message: "invalid request body"})
			return
		}

		resp, err := h(r.Context(), req)
		if err != nil {
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
