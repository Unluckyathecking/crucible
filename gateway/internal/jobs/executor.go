package jobs

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// jobsTracerName names the Tracer Executor.process starts jobs.execute spans
// from. Derived via otel.Tracer, which resolves to whatever provider is
// currently registered via otel.SetTracerProvider — see
// tracing.NewProvider's doc comment for why the async path uses the global
// registry instead of an explicit TracerProvider field: Executor has no
// constructor-injection point that main.go (outside this module's edit
// scope) could wire one through.
const jobsTracerName = "crucible.jobs"

// releaseTimeout bounds the per-job background writes (Complete/Fail) that
// must still land even after the caller's context is cancelled.
const releaseTimeout = 10 * time.Second

// completeMaxAttempts/completeRetryBackoff bound the retry of Store.Complete
// after a successful worker call. A worker success followed by a failed
// Complete write is worse than an ordinary write failure: the row stays
// 'running' with the result otherwise lost, and Claim's crash-recovery
// sweep will eventually re-execute the SAME job — a non-idempotent
// operation runs (and bills) twice. Retrying absorbs the common case (a
// transient Postgres blip); a sustained outage across all attempts still
// falls back to that same re-execution path, which this can't fully close
// without either a shared transaction (Record's signature can't change,
// see the comment on process's completion block) or a separate durable
// pending-finalization state — out of proportion for the failure window
// these retries already cover.
const (
	completeMaxAttempts  = 4
	completeRetryBackoff = 250 * time.Millisecond
)

// ExecutorConfig bounds the async Executor's concurrency and timing.
type ExecutorConfig struct {
	// PoolSize is the maximum number of jobs executed concurrently. <= 0
	// defaults to 4.
	PoolSize int
	// PollInterval is the delay between claim attempts. <= 0 defaults to 1s.
	PollInterval time.Duration
	// JobTimeout ceilings a single job's worker invocation. <= 0 defaults
	// to 5 minutes. NewExecutor also uses this as Store.DefaultJobTimeout's
	// value, so the executor's own budget and the store's stuck-job
	// recovery sweep can never disagree about how long a legitimately
	// running job is allowed to take.
	JobTimeout time.Duration
	// ErrorExposure mirrors config.Config.ErrorExposure ("sanitized" or
	// "full"). Any value other than "full" sanitizes worker-reported
	// structured errors before they're persisted to a failed job's
	// error_code/error_message — the same policy the synchronous /v1
	// invoke handler applies (server/routes.go) — so GET /v1/jobs/{id}
	// can't leak internal worker details an operator configured the
	// gateway to hide. Empty defaults to sanitized.
	ErrorExposure string
	// MaxAttempts bounds retries of a retryable (WORKER_UNREACHABLE /
	// transport) failure before the job dead-letters to terminal 'failed'.
	// <= 0 defaults to 3. Mirrors config.Config.JobMaxAttempts
	// (JOB_MAX_ATTEMPTS); a worker structured business error or a
	// billable_units<1 contract violation is never retried regardless of
	// this value — see process's classification of the two failure kinds.
	MaxAttempts int
	// RetryBackoff is the base delay before the first retry; each
	// subsequent retry doubles it, capped at maxRetryBackoff (bounded
	// exponential backoff — see retryBackoffDelay). <= 0 defaults to 2s.
	// Mirrors config.Config.JobRetryBackoffMS (JOB_RETRY_BACKOFF_MS).
	RetryBackoff time.Duration
	// MaxInflightPerCustomer bounds how many 'running' jobs a single
	// customer may occupy at once (see Store.Claim). <= 0 (the default)
	// disables the cap and preserves the original pure-FIFO claim query
	// byte-for-byte. Mirrors config.Config.JobMaxInflightPerCustomer
	// (JOB_MAX_INFLIGHT_PER_CUSTOMER). Deliberately NOT defaulted by
	// withDefaults below — unlike PoolSize/PollInterval/etc., zero is this
	// knob's meaningful "disabled" value, not a placeholder to promote.
	MaxInflightPerCustomer int
}

// defaultMaxAttempts/defaultRetryBackoff are ExecutorConfig's zero-value
// fallbacks, matching config.Config's JOB_MAX_ATTEMPTS/JOB_RETRY_BACKOFF_MS
// defaults so the async path retries transient failures out of the box even
// before an operator wires the env-configured values through.
const (
	defaultMaxAttempts  = 3
	defaultRetryBackoff = 2 * time.Second
	// maxRetryBackoff bounds the exponential growth of retryBackoffDelay so a
	// job with a generous MaxAttempts can't end up waiting an unreasonable
	// span between attempts.
	maxRetryBackoff = 5 * time.Minute
)

func (c ExecutorConfig) withDefaults() ExecutorConfig {
	if c.PoolSize <= 0 {
		c.PoolSize = 4
	}
	if c.PollInterval <= 0 {
		c.PollInterval = time.Second
	}
	if c.JobTimeout <= 0 {
		c.JobTimeout = 5 * time.Minute
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}
	if c.RetryBackoff <= 0 {
		c.RetryBackoff = defaultRetryBackoff
	}
	return c
}

// retryBackoffDelay returns the delay before attempt's retry: base doubled
// for each attempt past the first, capped at maxRetryBackoff. attempt is
// 1-based (the count of attempts made so far, i.e. the attempt that just
// failed) so the first retry (attempt=1) waits exactly base.
func retryBackoffDelay(base time.Duration, attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	// Cap the shift so 1<<uint(attempt-1) can't overflow into a negative or
	// wrapped duration for a pathologically large MaxAttempts.
	shift := attempt - 1
	if shift > 20 {
		shift = 20
	}
	d := base * time.Duration(int64(1)<<uint(shift))
	if d <= 0 || d > maxRetryBackoff {
		d = maxRetryBackoff
	}
	return d
}

// Executor claims queued jobs and invokes the existing worker contract for
// each — exactly as the synchronous /v1 path does via proxy.Client.Invoke —
// then meters through the existing usage.Recorder. One Executor per gateway
// process; instanceID scopes each claim's claimed_by column for
// observability (which instance is holding a given row) — see Run's doc
// comment for why claimed rows are never eagerly released back to a
// customer-visible 'queued' state by this instance itself.
type Executor struct {
	store      *Store
	proxy      *proxy.Client
	recorder   *usage.Recorder
	emitter    *webhookout.Emitter
	cfg        ExecutorConfig
	instanceID uuid.UUID
}

// NewExecutor constructs an Executor. store or p being nil makes Run a no-op
// (nil-safe, matching the framework's optional-Deps pattern) — this lets
// cmd/gateway/main.go construct the Executor unconditionally. The emitter
// starts nil (safe no-op for Emit); wire one in via SetEmitter.
func NewExecutor(store *Store, p *proxy.Client, recorder *usage.Recorder, cfg ExecutorConfig) *Executor {
	cfg = cfg.withDefaults()
	// Keep the store's stuck-job recovery threshold in sync with this
	// executor's own configured job timeout — see Store.DefaultJobTimeout's
	// doc comment for why a mismatch here is a duplicate-claim/double-bill
	// bug, not just a cosmetic inconsistency.
	if store != nil {
		store.DefaultJobTimeout = cfg.JobTimeout
	}
	return &Executor{
		store:      store,
		proxy:      p,
		recorder:   recorder,
		cfg:        cfg,
		instanceID: uuid.New(),
	}
}

// SetEmitter wires the outbound webhook emitter used to notify customers of
// job.succeeded/job.failed terminal transitions, mirroring
// auth.Store.SetEmitter and billing.Webhook.SetEmitter. cmd/gateway/main.go
// calls this with the SAME *webhookout.Emitter instance it injects into the
// router's Deps — one shared delivery worker, not a second Emitter. Passing
// nil (or never calling SetEmitter) is a safe no-op: Emitter.Emit nil-checks
// its own receiver.
func (e *Executor) SetEmitter(emitter *webhookout.Emitter) {
	e.emitter = emitter
}

// Run polls for queued jobs until ctx is cancelled, dispatching each claimed
// job into a bounded worker pool (ExecutorConfig.PoolSize). On cancellation
// it stops claiming new work and waits for in-flight process() calls to
// return, then returns itself — it does NOT eagerly release or requeue
// whatever it still has claimed. That is deliberate: the worker SDK only
// hands product code the request context and cannot force it to stop, so a
// job whose HTTP call was just abandoned client-side (context cancelled)
// may still be genuinely executing server-side. Marking it 'queued'
// immediately would let another claim (this instance after a restart, or
// another replica) start a second, concurrent execution of the same job
// while the first might still be running — double execution, and possibly
// double billing once both finish. Every claimed row is instead left
// exactly as Claim's crash-recovery sweep already treats an ungracefully
// killed process: 'running', to be reset to 'queued' only once its own
// timeout_seconds (or Store.DefaultJobTimeout) plus stuckJobGrace has
// genuinely elapsed — by which point the original call has either finished
// or is long past its own deadline. "No lost work" still holds; it just
// isn't instant. Run blocks until shutdown is complete; start it in its own
// goroutine (mirrors usage.Flusher.Run).
func (e *Executor) Run(ctx context.Context) {
	if e == nil || e.store == nil || e.proxy == nil {
		return
	}

	sem := make(chan struct{}, e.cfg.PoolSize)
	var wg sync.WaitGroup

	ticker := time.NewTicker(e.cfg.PollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			e.claimAndDispatch(ctx, sem, &wg)
		}
	}
}

func (e *Executor) claimAndDispatch(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	// Refreshed once per poll tick regardless of whether fairness is
	// enabled — a cheap, index-backed COUNT (idx_async_jobs_queued) that
	// gives operators queue-depth visibility even on the default unbounded
	// FIFO path. A failure here is observability-only, never fatal to
	// claiming/dispatching.
	if depth, err := e.store.QueueDepth(ctx); err == nil {
		observability.JobsQueueDepth.Set(float64(depth))
	}

	free := cap(sem) - len(sem)
	if free <= 0 {
		return
	}
	claimed, err := e.store.Claim(ctx, free, e.instanceID, e.cfg.MaxInflightPerCustomer)
	if err != nil {
		log.Warn().Err(err).Msg("jobs: claim failed")
		return
	}
	if len(claimed) == 0 {
		return
	}

	// One batch lookup per claim tick, not per job — see
	// Store.TraceparentsByID's doc comment for why this is a separate query
	// rather than a Claim-returned field. A lookup failure degrades to
	// tracing loss for this batch, never to dropping the claimed work itself:
	// traceparents left nil makes every process() call below restore a
	// no-op parent (tracing.RestoreTraceparent("") is exact for that).
	ids := make([]uuid.UUID, len(claimed))
	for i, j := range claimed {
		ids[i] = j.ID
	}
	traceparents, err := e.store.TraceparentsByID(ctx, ids)
	if err != nil {
		log.Warn().Err(err).Msg("jobs: fetch traceparents failed")
	}

	for _, j := range claimed {
		sem <- struct{}{}
		wg.Add(1)
		go func(job Job) {
			defer wg.Done()
			defer func() { <-sem }()
			e.process(ctx, job, traceparents[job.ID])
		}(j)
	}
}

// process invokes the worker for a single claimed job and records the
// outcome. runCtx is the Executor.Run-scoped context (cancelled at
// shutdown); a per-job timeout is derived from it so a hung worker cannot
// block a pool slot forever. traceparent is job's captured enqueue-time
// trace context (from Store.TraceparentsByID), "" when none was captured.
//
// The jobs.execute span started below is restored onto runCtx, not derived
// from it: runCtx comes from Executor.Run's caller (main.go's background
// context), which never carries a span of its own — the whole point of this
// module is that the async path is otherwise completely dark to tracing.
// Once jobCtx (derived from the span-bearing context) is passed to
// e.proxy.Invoke, proxy.Client.doOnce's existing
// oteltrace.SpanFromContext(ctx).TracerProvider() derivation picks up this
// same real provider automatically and proxy.invoke nests under jobs.execute
// — proxy/client.go needs no changes for that to happen.
func (e *Executor) process(runCtx context.Context, job Job, traceparent string) {
	start := time.Now()
	defer func() {
		observability.JobExecutionDuration.WithLabelValues(job.Operation).Observe(time.Since(start).Seconds())
	}()

	execCtx := tracing.RestoreTraceparent(runCtx, traceparent)
	execCtx, span := otel.Tracer(jobsTracerName).Start(execCtx, "jobs.execute")
	span.SetAttributes(
		attribute.String("job.id", job.ID.String()),
		attribute.String("job.operation", job.Operation),
	)
	defer span.End()

	timeout := e.cfg.JobTimeout
	if job.TimeoutSeconds > 0 {
		timeout = time.Duration(job.TimeoutSeconds) * time.Second
	}
	jobCtx, cancel := context.WithTimeout(execCtx, timeout)
	defer cancel()

	req := &proxy.InvokeRequest{
		RequestID:  job.RequestID,
		CustomerID: job.CustomerID.String(),
		Operation:  job.Operation,
		Payload:    job.Payload,
		Plan:       job.Plan,
	}
	resp, err := e.proxy.Invoke(jobCtx, req)
	if err != nil {
		if runCtx.Err() != nil {
			// Shutdown in progress, not necessarily a genuine worker
			// failure: leave the row 'running' and do nothing further. See
			// Run's doc comment for why an immediate requeue here would
			// risk a second, concurrent execution of a job whose worker
			// call may still genuinely be in flight — the crash-recovery
			// sweep in Claim is the safe path back to 'queued'.
			return
		}
		log.Error().Err(err).Str("job_id", job.ID.String()).Str("operation", job.Operation).
			Msg("jobs: worker invocation failed")
		observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_UNREACHABLE).Inc()
		span.RecordError(err)
		span.SetStatus(codes.Error, apierror.WORKER_UNREACHABLE)
		// Retryable: a transport/WORKER_UNREACHABLE failure is exactly the
		// class of error that clears on its own once a restarted worker or a
		// network blip recovers — see retryOrDeadLetter's doc comment for why
		// this is the only failure kind Executor ever retries.
		e.retryOrDeadLetter(job, apierror.WORKER_UNREACHABLE, "worker unavailable")
		return
	}

	if resp.Error != nil {
		metricCode := resp.Error.Code
		if metricCode == "" {
			metricCode = apierror.UNKNOWN
		}
		observability.WorkerErrorsTotal.WithLabelValues(metricCode).Inc()
		// Persist the sanitized (code, message), never the worker's raw
		// values verbatim — GET /v1/jobs/{id} returns these to the customer,
		// and must honor WORKER_ERROR_EXPOSURE exactly like the synchronous
		// /v1 invoke handler does. Deterministic: the worker itself reported
		// a structured business error, so retrying would just waste another
		// call for the same outcome — fail immediately, attempts untouched.
		sanitizedCode, sanitizedMsg := SanitizeWorkerError(e.cfg.ErrorExposure, resp.Error.Code, resp.Error.Message)
		// The span is an internal observability surface, not the
		// customer-facing job record, so it carries the worker's raw error
		// detail regardless of WORKER_ERROR_EXPOSURE — that policy only
		// governs what GET /v1/jobs/{id} returns to the customer.
		span.SetStatus(codes.Error, resp.Error.Code+": "+resp.Error.Message)
		e.fail(job, sanitizedCode, sanitizedMsg)
		return
	}

	// Contract check: reuses the exact predicate the synchronous /v1 invoke
	// handler uses (server/routes.go), not a second copy of the rule.
	// Deterministic: the worker will report the same violation on any retry
	// (invariant #2's trust boundary, not a transient blip) — fail
	// immediately, attempts untouched, exactly like a structured business
	// error above.
	if !ValidBillableUnits(resp.BillableUnits) {
		log.Warn().Str("job_id", job.ID.String()).Str("operation", job.Operation).
			Msg("jobs: worker returned success with billable_units<1 — rejecting")
		observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_BAD_RESPONSE).Inc()
		span.SetStatus(codes.Error, "worker contract violation: billable_units < 1")
		e.fail(job, apierror.WORKER_BAD_RESPONSE, "worker contract violation")
		return
	}

	// Complete then Record are two separate writes, not one transaction —
	// usage.Recorder.Record owns its own db.Exec and, per invariant #4, its
	// signature can't change to accept a shared tx, so true atomicity would
	// mean reimplementing its insert (and the quota.Add/MarkRecorded side
	// effects it performs) here instead of reusing it, which is worse: a
	// second, drift-prone copy of the billing write path. The ordering
	// below is deliberate given that constraint: marking 'succeeded' first
	// means a process kill between the two writes can only under-bill (the
	// job shows as succeeded with no usage_events row — a bounded, rare
	// revenue leak), never double-bill — a 'succeeded' row is terminal and
	// is never reclaimed by Claim, so Record can't run twice for it. The
	// reverse order would close the under-billing gap but reopen a
	// double-billing one: if Record lands but the crash happens before
	// Complete, the row is still 'running' and the crash-recovery sweep
	// will eventually reclaim and re-execute it, calling Record a second
	// time. Between a rare silent discount and a rare double charge, this
	// keeps the fail-safe direction on the side that doesn't harm a
	// customer.
	bg, bgCancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer bgCancel()
	if err := e.complete(job.ID, resp.Payload, resp.BillableUnits, resp.UnitsLabel); err != nil {
		log.Error().Err(err).Str("job_id", job.ID.String()).
			Msg("jobs: mark complete failed after retries — job remains 'running' and the worker's result is lost; the crash-recovery sweep will re-execute it")
		return
	}
	observability.JobsCompletedTotal.WithLabelValues(job.Operation, "succeeded").Inc()
	if e.recorder != nil {
		if err := e.recorder.Record(bg, job.CustomerID, job.APIKeyID, job.Operation, job.RequestID, resp.BillableUnits); err != nil {
			log.Warn().Err(err).Str("job_id", job.ID.String()).Msg("jobs: usage record failed")
		}
	}
	// A fresh timeout, not bg: bg's clock started before e.complete's retry
	// loop (up to completeMaxAttempts attempts) and recorder.Record, both of
	// which can consume most or all of releaseTimeout before this point —
	// notifySucceeded would then run against an already-expired context,
	// silently dropping the webhook (Emit only logs on error) even though
	// the job itself completed successfully.
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer notifyCancel()
	notifySucceeded(notifyCtx, e.emitter, job)
}

// complete retries Store.Complete up to completeMaxAttempts times with a
// fixed backoff between attempts. See completeMaxAttempts's doc comment for
// why a worker success followed by a failed Complete write deserves more
// resilience than an ordinary background write.
func (e *Executor) complete(id uuid.UUID, payload json.RawMessage, billableUnits uint64, unitsLabel string) error {
	var lastErr error
	for attempt := 0; attempt < completeMaxAttempts; attempt++ {
		if attempt > 0 {
			time.Sleep(completeRetryBackoff)
		}
		bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		err := e.store.Complete(bg, id, payload, billableUnits, unitsLabel)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		log.Warn().Err(err).Str("job_id", id.String()).Int("attempt", attempt+1).
			Msg("jobs: mark complete failed, retrying")
	}
	return lastErr
}

func (e *Executor) fail(job Job, code, message string) {
	bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := e.store.Fail(bg, job.ID, code, message); err != nil {
		log.Error().Err(err).Str("job_id", job.ID.String()).Msg("jobs: mark failed failed")
		return
	}
	observability.JobsCompletedTotal.WithLabelValues(job.Operation, "failed").Inc()
	// A fresh timeout, not bg: see notifySucceeded's call site in process for
	// why reusing a context whose clock started before the terminal DB write
	// risks handing notifyFailed an already-(near-)expired context.
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer notifyCancel()
	notifyFailed(notifyCtx, e.emitter, job, code)
}

// retryOrDeadLetter handles the ONLY failure kind Executor ever retries: a
// WORKER_UNREACHABLE / transport error, where the worker itself never ran
// (so nothing was billed and nothing needs undoing) and the failure is
// plausibly transient — a worker mid-restart, a brief network blip — unlike
// a worker's own structured business error or a billable_units<1 contract
// violation, both of which are deterministic and handled by fail() directly
// at their call sites in process().
//
// job.Attempts is the count already made (from Claim's SELECT); if
// incrementing it is still below cfg.MaxAttempts the job returns to
// 'queued' via Store.RequeueRetry with its next_attempt_at pushed out by
// retryBackoffDelay, to be claimed again once eligible. Once exhausted it
// dead-letters to terminal 'failed' via Store.DeadLetter — the same
// customer-visible shape process()'s other failure paths already produce
// (GET /v1/jobs/{id} never gains a new status).
func (e *Executor) retryOrDeadLetter(job Job, code, message string) {
	newAttempts := job.Attempts + 1
	if newAttempts < e.cfg.MaxAttempts {
		delay := retryBackoffDelay(e.cfg.RetryBackoff, newAttempts)
		bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
		defer cancel()
		if err := e.store.RequeueRetry(bg, job.ID, newAttempts, time.Now().Add(delay)); err != nil {
			log.Error().Err(err).Str("job_id", job.ID.String()).Msg("jobs: requeue retry failed")
			return
		}
		observability.JobsRetriedTotal.WithLabelValues(job.Operation).Inc()
		log.Warn().Str("job_id", job.ID.String()).Str("operation", job.Operation).
			Int("attempt", newAttempts).Dur("delay", delay).
			Msg("jobs: transient worker failure, scheduled retry")
		return
	}

	bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := e.store.DeadLetter(bg, job.ID, newAttempts, code, message); err != nil {
		log.Error().Err(err).Str("job_id", job.ID.String()).Msg("jobs: dead letter failed")
		return
	}
	observability.JobsCompletedTotal.WithLabelValues(job.Operation, "failed").Inc()
	observability.JobsDeadletteredTotal.WithLabelValues(job.Operation).Inc()
	log.Warn().Str("job_id", job.ID.String()).Str("operation", job.Operation).
		Int("attempts", newAttempts).Msg("jobs: retries exhausted, dead-lettered")
	// A fresh timeout — see fail's call site for why.
	notifyCtx, notifyCancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer notifyCancel()
	notifyFailed(notifyCtx, e.emitter, job, code)
}
