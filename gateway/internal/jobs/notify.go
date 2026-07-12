package jobs

import (
	"context"
	"encoding/json"

	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// notifySucceeded fires a best-effort job.succeeded webhook for a job that
// just reached the terminal 'succeeded' state. emitter is a
// *webhookout.Emitter, which nil-checks its own receiver — the same
// nil-safe-optional-dep pattern as every other Emit call-site — so a nil
// emitter (Deps.DB unset) is a no-op here with no extra check needed.
func notifySucceeded(ctx context.Context, emitter *webhookout.Emitter, job Job) {
	payload, err := json.Marshal(events.JobSucceededPayload{
		JobID:     job.ID.String(),
		Operation: job.Operation,
		Status:    StatusSucceeded,
	})
	if err != nil {
		log.Warn().Err(err).Str("job_id", job.ID.String()).Msg("webhook emit: job.succeeded payload marshal failed")
		return
	}
	if err := emitter.Emit(ctx, job.CustomerID, events.JobSucceeded, payload); err != nil {
		log.Warn().Err(err).Str("job_id", job.ID.String()).Msg("webhook emit failed for job.succeeded")
	}
}

// notifyFailed fires a best-effort job.failed webhook for a job that just
// reached the terminal 'failed' state — a worker-reported structured error, a
// billable_units<1 contract violation, or retry-exhausted dead-letter.
// errorCode is always the already-sanitized (or full, per
// WORKER_ERROR_EXPOSURE) code the job row itself carries, never the worker's
// raw result body. See notifySucceeded's doc comment for why a nil emitter
// needs no extra check here.
func notifyFailed(ctx context.Context, emitter *webhookout.Emitter, job Job, errorCode string) {
	payload, err := json.Marshal(events.JobFailedPayload{
		JobID:     job.ID.String(),
		Operation: job.Operation,
		Status:    StatusFailed,
		ErrorCode: errorCode,
	})
	if err != nil {
		log.Warn().Err(err).Str("job_id", job.ID.String()).Msg("webhook emit: job.failed payload marshal failed")
		return
	}
	if err := emitter.Emit(ctx, job.CustomerID, events.JobFailed, payload); err != nil {
		log.Warn().Err(err).Str("job_id", job.ID.String()).Msg("webhook emit failed for job.failed")
	}
}
