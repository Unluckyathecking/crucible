package jobs

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/apierror"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

// releaseTimeout bounds the final ReleaseClaimed safety-net write during
// Executor.Run's shutdown, and the per-job background writes (Complete/Fail/
// Requeue) that must still land even after the caller's context is cancelled.
const releaseTimeout = 10 * time.Second

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
}

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
	return c
}

// Executor claims queued jobs and invokes the existing worker contract for
// each — exactly as the synchronous /v1 path does via proxy.Client.Invoke —
// then meters through the existing usage.Recorder. One Executor per gateway
// process; instanceID scopes claimed_by so a graceful shutdown only ever
// releases jobs this process claimed, never another replica's in-flight work.
type Executor struct {
	store      *Store
	proxy      *proxy.Client
	recorder   *usage.Recorder
	cfg        ExecutorConfig
	instanceID uuid.UUID
}

// NewExecutor constructs an Executor. store or p being nil makes Run a no-op
// (nil-safe, matching the framework's optional-Deps pattern) — this lets
// cmd/gateway/main.go construct the Executor unconditionally.
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

// Run polls for queued jobs until ctx is cancelled, dispatching each claimed
// job into a bounded worker pool (ExecutorConfig.PoolSize). On cancellation
// it stops claiming new work, waits for in-flight jobs to observe the
// cancelled context and exit, then releases (back to 'queued') any job still
// claimed by this instance — no lost work. Run blocks until shutdown is
// complete; start it in its own goroutine (mirrors usage.Flusher.Run).
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
			e.releaseClaimed()
			return
		case <-ticker.C:
			e.claimAndDispatch(ctx, sem, &wg)
		}
	}
}

func (e *Executor) claimAndDispatch(ctx context.Context, sem chan struct{}, wg *sync.WaitGroup) {
	free := cap(sem) - len(sem)
	if free <= 0 {
		return
	}
	claimed, err := e.store.Claim(ctx, free, e.instanceID)
	if err != nil {
		log.Warn().Err(err).Msg("jobs: claim failed")
		return
	}
	for _, j := range claimed {
		sem <- struct{}{}
		wg.Add(1)
		go func(job Job) {
			defer wg.Done()
			defer func() { <-sem }()
			e.process(ctx, job)
		}(j)
	}
}

func (e *Executor) releaseClaimed() {
	bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	n, err := e.store.ReleaseClaimed(bg, e.instanceID)
	if err != nil {
		log.Warn().Err(err).Msg("jobs: release claimed on shutdown failed")
		return
	}
	if n > 0 {
		log.Info().Int64("count", n).Msg("jobs: released claimed jobs on shutdown")
	}
}

// process invokes the worker for a single claimed job and records the
// outcome. runCtx is the Executor.Run-scoped context (cancelled at
// shutdown); a per-job timeout is derived from it so a hung worker cannot
// block a pool slot forever.
func (e *Executor) process(runCtx context.Context, job Job) {
	start := time.Now()
	defer func() {
		observability.JobExecutionDuration.WithLabelValues(job.Operation).Observe(time.Since(start).Seconds())
	}()

	timeout := e.cfg.JobTimeout
	if job.TimeoutSeconds > 0 {
		timeout = time.Duration(job.TimeoutSeconds) * time.Second
	}
	jobCtx, cancel := context.WithTimeout(runCtx, timeout)
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
			// Shutdown in progress, not a genuine worker failure: requeue so
			// this job is retried instead of permanently failing the
			// customer's request. No lost work.
			e.requeue(job.ID, job.Operation)
			return
		}
		log.Error().Err(err).Str("job_id", job.ID.String()).Str("operation", job.Operation).
			Msg("jobs: worker invocation failed")
		observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_UNREACHABLE).Inc()
		e.fail(job.ID, job.Operation, apierror.WORKER_UNREACHABLE, "worker unavailable")
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
		// /v1 invoke handler does.
		sanitizedCode, sanitizedMsg := SanitizeWorkerError(e.cfg.ErrorExposure, resp.Error.Code, resp.Error.Message)
		e.fail(job.ID, job.Operation, sanitizedCode, sanitizedMsg)
		return
	}

	// Contract check: reuses the exact predicate the synchronous /v1 invoke
	// handler uses (server/routes.go), not a second copy of the rule.
	if !ValidBillableUnits(resp.BillableUnits) {
		log.Warn().Str("job_id", job.ID.String()).Str("operation", job.Operation).
			Msg("jobs: worker returned success with billable_units<1 — rejecting")
		observability.WorkerErrorsTotal.WithLabelValues(apierror.WORKER_BAD_RESPONSE).Inc()
		e.fail(job.ID, job.Operation, apierror.WORKER_BAD_RESPONSE, "worker contract violation")
		return
	}

	bg, bgCancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer bgCancel()
	if err := e.store.Complete(bg, job.ID, resp.Payload, resp.BillableUnits, resp.UnitsLabel); err != nil {
		log.Error().Err(err).Str("job_id", job.ID.String()).Msg("jobs: mark complete failed")
		return
	}
	observability.JobsCompletedTotal.WithLabelValues(job.Operation, "succeeded").Inc()
	if e.recorder != nil {
		if err := e.recorder.Record(bg, job.CustomerID, job.APIKeyID, job.Operation, job.RequestID, resp.BillableUnits); err != nil {
			log.Warn().Err(err).Str("job_id", job.ID.String()).Msg("jobs: usage record failed")
		}
	}
}

func (e *Executor) fail(id uuid.UUID, operation, code, message string) {
	bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := e.store.Fail(bg, id, code, message); err != nil {
		log.Error().Err(err).Str("job_id", id.String()).Msg("jobs: mark failed failed")
		return
	}
	observability.JobsCompletedTotal.WithLabelValues(operation, "failed").Inc()
}

func (e *Executor) requeue(id uuid.UUID, operation string) {
	bg, cancel := context.WithTimeout(context.Background(), releaseTimeout)
	defer cancel()
	if err := e.store.Requeue(bg, id); err != nil {
		log.Error().Err(err).Str("job_id", id.String()).Msg("jobs: requeue on shutdown failed")
		return
	}
	observability.JobsCompletedTotal.WithLabelValues(operation, "requeued").Inc()
}
