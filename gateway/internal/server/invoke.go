package server

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	mw "github.com/Unluckyathecking/crucible/gateway/internal/middleware"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

func invoke(p *proxy.Client, recorder *usage.Recorder, errorExposure string, operation string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rid := mw.RequestIDFrom(r.Context())
		key := auth.FromContext(r.Context())

		var payload json.RawMessage
		// r.Body is already wrapped by http.MaxBytesReader via mw.BodyLimit at the router level;
		// reads beyond cfg.BodyLimitBytes return an error the decoder propagates here as BAD_REQUEST.
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			apierror.Write(w, rid, http.StatusBadRequest, apierror.BAD_REQUEST, "invalid json body", false) // false: malformed JSON is permanent; retrying with the same body will not succeed
			return
		}

		req := &proxy.InvokeRequest{RequestID: rid, Operation: operation, Payload: payload}
		// key is nil in tests that bypass auth.Middleware; skip customer fields safely.
		if key != nil {
			req.CustomerID = key.Customer.ID.String()
			req.Plan = key.Customer.Plan
		}

		resp, err := p.Invoke(r.Context(), req)
		if err != nil {
			log.Error().
				Err(err).
				Str("request_id", rid).
				Str("operation", operation).
				Msg("worker invocation failed")
			observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_UNREACHABLE).Inc()
			apierror.Write(w, rid, http.StatusBadGateway, apierror.WORKER_UNREACHABLE, "worker unavailable", true)
			return
		}

		// Worker structured errors: expose or sanitize based on config.
		if resp.Error != nil {
			// Guard the metric label against an empty Code from a buggy or non-SDK worker:
			// an empty Code would open an unlabelled code="" series.
			metricCode := resp.Error.Code
			if metricCode == "" {
				metricCode = apierror.UNKNOWN
			}
			observability.WorkerErrorsTotal.WithLabelValues(metricCode).Inc()
			// Sanitized mode always returns WORKER_UNREACHABLE — the customer-facing
			// contract must not change regardless of what the worker sends.
			// Full mode passes the worker's error verbatim; guard empty Code so
			// customers never receive "code":"" (an empty code is not correlatable).
			// jobs.SanitizeWorkerError is the single definition of this policy — the
			// async Executor applies it identically before persisting a failed job's
			// error_code/error_message, so GET /v1/jobs/{id} can't leak what this
			// path hides.
			errCode, errMsg := jobs.SanitizeWorkerError(errorExposure, resp.Error.Code, resp.Error.Message)
			errRetryable := true
			if errorExposure == "full" {
				errRetryable = resp.Error.Retryable
			}
			apierror.Write(w, rid, http.StatusBadGateway, errCode, errMsg, errRetryable)
			return
		}

		// Contract check: a successful worker response MUST report billable_units >= 1.
		// Otherwise a buggy or malicious non-SDK worker could let customers consume service for free.
		// The SDK enforces this client-side, but the gateway is the trust boundary.
		if resp.BillableUnits < 1 {
			log.Warn().
				Str("request_id", rid).
				Str("operation", operation).
				Msg("worker returned success with billable_units<1 — rejecting")
			observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_BAD_RESPONSE).Inc()
			apierror.Write(w, rid, http.StatusBadGateway, apierror.WORKER_BAD_RESPONSE, "worker contract violation", false)
			return
		}

		// Record usage on successful (non-error) responses. Best-effort; do not fail the customer on write error.
		// key is nil in tests that bypass auth.Middleware; skip customer fields safely.
		if key != nil {
			if err := recorder.Record(r.Context(), key.Customer.ID, key.ID, operation, rid, resp.BillableUnits); err != nil {
				log.Warn().Err(err).Str("request_id", rid).Msg("usage record failed")
			}
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if resp.BillableUnits > 0 {
			w.Header().Set("X-Billable-Units", strconv.FormatUint(resp.BillableUnits, 10))
		}
		if resp.UnitsLabel != "" {
			w.Header().Set("X-Units-Label", resp.UnitsLabel)
		}
		_ = json.NewEncoder(w).Encode(resp)
	}
}
