// Delivery and outcome recording for the webhook emitter.

package webhookout

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"

	"github.com/Unluckyathecking/crucible/gateway/internal/events"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
)

func (e *Emitter) deliver(ctx context.Context, r pendingRow) {
	// execCtx continues the trace that started at Emit's enqueue call (see
	// tracing.RestoreTraceparent); "" is a no-op, leaving ctx unchanged —
	// tracing disabled, or the row predates this column.
	execCtx := tracing.RestoreTraceparent(ctx, r.traceparent)
	execCtx, span := otel.Tracer(webhookoutTracerName).Start(execCtx, "webhook.deliver")
	span.SetAttributes(
		attribute.String("webhook.event_id", r.eventID),
		attribute.String("webhook.event_type", r.eventType),
	)
	defer span.End()

	reqCtx, cancel := context.WithTimeout(execCtx, deliveryTimeout)
	defer cancel()

	ts := strconv.FormatInt(time.Now().UTC().Unix(), 10)
	sig := Sign(r.secret, ts, r.payload)

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, r.url, bytes.NewReader(r.payload))
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		// URL is permanently malformed; dead-letter immediately, no HTTP status.
		e.markDeadLetter(r.id, r.attempts+1, nil)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Crucible-Timestamp", ts)
	req.Header.Set("X-Crucible-Signature", "t="+ts+",v1="+sig)
	req.Header.Set("X-Webhook-Event-ID", r.eventID)
	req.Header.Set("X-Webhook-Event-Type", r.eventType)
	// Propagate W3C traceparent when a span is active in execCtx; no-op when
	// absent (disabled path). The X-Crucible-*/X-Webhook-* headers above are
	// not removed or modified.
	deliverPropagator.Inject(execCtx, propagation.HeaderCarrier(req.Header))

	resp, doErr := e.client.Do(req)
	if doErr == nil {
		// Drain before close so the transport can reuse the TCP connection.
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}

	newAttempts := r.attempts + 1
	if doErr == nil && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		e.markDelivered(r.id, r.endpointID, newAttempts, resp.StatusCode)
		return
	}

	if doErr != nil {
		span.RecordError(doErr)
		span.SetStatus(codes.Error, doErr.Error())
	} else {
		span.SetStatus(codes.Error, fmt.Sprintf("webhook endpoint returned HTTP %d", resp.StatusCode))
	}

	// nil means no HTTP response (network error); non-nil is the actual status code.
	var statusCode *int
	if doErr == nil {
		sc := resp.StatusCode
		statusCode = &sc
	}

	if newAttempts >= maxAttempts {
		e.markDeadLetter(r.id, newAttempts, statusCode)
		return
	}
	e.scheduleRetry(r.id, newAttempts, statusCode)
}

// markDelivered, markDeadLetter, scheduleRetry use a fresh background-derived context
// so a shutting-down worker context does not prevent recording the delivery outcome.

func (e *Emitter) markDelivered(id int64, endpointID uuid.UUID, attempts, statusCode int) {
	if e.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbWriteTimeout)
	defer cancel()
	_, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries
		SET status = 'delivered', attempts = $2, last_response_code = $3, claimed_at = NULL
		WHERE id = $1
	`, id, attempts, statusCode)
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: mark delivered failed")
	}
	// Health accounting costs one extra round trip per terminal outcome, so
	// it is skipped entirely while auto-disable is off (threshold <= 0, the
	// default) — a deployment that never opts in pays nothing for the
	// feature. The counter only moves while the knob is on, which is also the
	// only window in which anything reads it.
	if e.failureThreshold.Load() <= 0 {
		return
	}
	if err := recordDeliverySuccess(ctx, e.db, endpointID); err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Str("endpoint_id", endpointID.String()).Msg("webhookout: reset endpoint failure counter failed")
	}
}

func (e *Emitter) markDeadLetter(id int64, attempts int, statusCode *int) {
	if e.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbWriteTimeout)
	defer cancel()
	// RETURNING we.id both confirms the UPDATE actually matched a row (the
	// pre-existing unsubscribed-row fallback below) and hands back the
	// endpoint id for the health accounting that follows — without needing
	// endpointID threaded in as a parameter, keeping this method's signature
	// unchanged for every existing caller.
	var endpointID uuid.UUID
	err := e.db.QueryRow(ctx, `
		UPDATE webhook_deliveries AS d
		SET status = 'dead_letter', attempts = $2, last_response_code = $3, claimed_at = NULL
		FROM webhook_endpoints AS we
		WHERE d.id = $1 AND we.id = d.endpoint_id
		  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
		RETURNING we.id
	`, id, attempts, statusCode).Scan(&endpointID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Row was for an event type the endpoint no longer subscribes to
			// — not a genuine delivery failure of the endpoint itself, so it
			// doesn't count toward auto-disable.
			if derr := e.deleteUnsubscribedRow(ctx, id); derr != nil {
				log.Warn().Err(derr).Int64("delivery_id", id).Msg("webhookout: mark dead_letter failed")
			}
			return
		}
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: mark dead_letter failed")
		return
	}

	// No health round trip while auto-disable is off — see markDelivered.
	threshold := int(e.failureThreshold.Load())
	if threshold <= 0 {
		return
	}
	custID, justDisabled, herr := recordDeliveryFailure(ctx, e.db, endpointID, threshold)
	if herr != nil {
		log.Warn().Err(herr).Int64("delivery_id", id).Str("endpoint_id", endpointID.String()).Msg("webhookout: record endpoint failure failed")
		return
	}
	if !justDisabled {
		return
	}
	observability.WebhookEndpointsDisabledTotal.Inc()
	payload, merr := json.Marshal(events.EndpointDisabledPayload{
		EndpointID: endpointID.String(),
		Reason:     DisabledReasonDeliveryFailures,
	})
	if merr != nil {
		log.Warn().Err(merr).Str("endpoint_id", endpointID.String()).Msg("webhookout: marshal endpoint.disabled payload failed")
		return
	}
	// customerID's other active endpoints only — Emit's own active = TRUE
	// filter already self-excludes the endpoint just disabled above.
	if eerr := e.Emit(ctx, custID, events.EndpointDisabled, payload); eerr != nil {
		log.Warn().Err(eerr).Str("endpoint_id", endpointID.String()).Msg("webhookout: emit endpoint.disabled failed")
	}
}

func (e *Emitter) scheduleRetry(id int64, newAttempts int, statusCode *int) {
	if e.db == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), dbWriteTimeout)
	defer cancel()
	// backoffSchedule index is the 0-based attempt count that JUST failed.
	// newAttempts = failedAttempts + 1, so the index is newAttempts - 1.
	idx := newAttempts - 1
	if idx >= len(backoffSchedule) {
		idx = len(backoffSchedule) - 1
	}
	nextAt := time.Now().Add(backoffSchedule[idx])
	tag, err := e.db.Exec(ctx, `
		UPDATE webhook_deliveries AS d
		SET status = 'pending', attempts = $2, next_attempt_at = $3, last_response_code = $4, claimed_at = NULL
		FROM webhook_endpoints AS we
		WHERE d.id = $1 AND we.id = d.endpoint_id
		  AND (we.subscribed_events IS NULL OR d.event_type = ANY(we.subscribed_events))
	`, id, newAttempts, nextAt, statusCode)
	if err == nil && tag.RowsAffected() == 0 {
		err = e.deleteUnsubscribedRow(ctx, id)
	}
	if err != nil {
		log.Warn().Err(err).Int64("delivery_id", id).Msg("webhookout: schedule retry failed")
	}
}

// deleteUnsubscribedRow removes a webhook_deliveries row whose recording UPDATE
// (scheduleRetry/markDeadLetter) matched zero rows because the endpoint's
// subscription no longer includes this row's event type. This is the same
// narrowing the customer already opted into via updateWebhookEndpointSubscription
// (dashboard/lib/db.ts) — without it, a row whose delivery attempt was already
// in flight when the subscription changed would survive that cleanup and could
// be delivered again if the customer later re-subscribed to the dropped type.
func (e *Emitter) deleteUnsubscribedRow(ctx context.Context, id int64) error {
	_, err := e.db.Exec(ctx, `DELETE FROM webhook_deliveries WHERE id = $1`, id)
	return err
}
