// Crucible gateway entry point.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/cache"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/idempotency"
	"github.com/Unluckyathecking/crucible/gateway/internal/jobs"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/operator"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/ratelimit"
	"github.com/Unluckyathecking/crucible/gateway/internal/respcache"
	"github.com/Unluckyathecking/crucible/gateway/internal/runtime"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
	"github.com/Unluckyathecking/crucible/gateway/internal/webhookout"
)

// redisPinger adapts *redis.Client to server.HealthChecker.
type redisPinger struct{ c *redis.Client }

func (r *redisPinger) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }

// pgPinger adapts *pgxpool.Pool to server.HealthChecker.
type pgPinger struct{ p *pgxpool.Pool }

func (p *pgPinger) Ping(ctx context.Context) error { return p.p.Ping(ctx) }

func main() {
	// Best-effort .env load; absent .env is fine if env is set externally (CI, docker, prod).
	_ = godotenv.Load()

	// godotenv loads values literally, so a placeholder like
	//   POSTGRES_DSN=postgres://crucible:${POSTGRES_PASSWORD}@.../crucible
	// is left as-is. Docker Compose natively expands ${VAR} so the container deploy works.
	// For local `go run` workflows we expand only the known interpolating connection-string
	// variables — and ONLY when they actually contain a ${...} placeholder.
	//
	// Skipping ExpandEnv when no placeholder is present matters because os.ExpandEnv would
	// otherwise corrupt a fully-resolved value with literal $-signs (e.g. a generated
	// password like "MyP@$$w0rd" — $$ would be eaten as a malformed reference). Production
	// configs usually inject fully-resolved DSN/URLs via env vars, so this expansion must
	// be opt-in via the placeholder syntax.
	for _, key := range []string{"POSTGRES_DSN", "REDIS_URL", "WORKER_URL"} {
		v := os.Getenv(key)
		if v != "" && strings.Contains(v, "${") {
			_ = os.Setenv(key, os.ExpandEnv(v))
		}
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}

	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix
	if lvl, err := zerolog.ParseLevel(cfg.LogLevel); err == nil {
		zerolog.SetGlobalLevel(lvl)
	}

	rootCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	pool, err := db.NewPool(rootCtx, cfg.PostgresDSN, cfg.PostgresMaxConns)
	if err != nil {
		log.Fatal().Err(err).Msg("postgres unavailable")
	}
	defer pool.Close()

	if err := db.Apply(rootCtx, pool); err != nil {
		log.Fatal().Err(err).Msg("migration failed")
	}

	redisClient, err := cache.NewRedis(rootCtx, cfg.RedisURL)
	if err != nil {
		log.Fatal().Err(err).Msg("redis unavailable")
	}
	defer func() { _ = redisClient.Close() }()

	components, err := runtime.Assemble(rootCtx, cfg)
	if err != nil {
		log.Fatal().Err(err).Msg("runtime assembly failed")
	}

	authStore := auth.NewStore(pool, redisClient, cfg.APIKeyHashSalt)
	// Split GATEWAY_WORKER_MAX_CONNS between the synchronous and async
	// worker clients so their combined connection ceiling to the worker
	// never exceeds what the operator configured — each gets its own
	// http.Transport (proxy.New's timeout argument means the two clients
	// can't safely share one, see jobProxyClient below), and naively
	// giving both the full WorkerMaxConns would let a single gateway
	// process open up to 2x the configured cap. The async share is sized
	// to JobWorkerPoolSize, the executor's own concurrency cap — it can
	// never actually use more connections than that at once regardless of
	// a larger allowance — leaving the remainder for synchronous traffic.
	// Config.Load already guarantees WorkerMaxConns >= 1.
	asyncMaxConns := cfg.JobWorkerPoolSize
	if asyncMaxConns < 1 {
		asyncMaxConns = 1
	}
	if asyncMaxConns > cfg.WorkerMaxConns-1 {
		asyncMaxConns = cfg.WorkerMaxConns - 1
	}
	if asyncMaxConns < 1 {
		// WorkerMaxConns is pathologically small (<= 1): fall back to 1
		// rather than passing 0 to proxy.New, which treats <= 0 as "use its
		// own 64-connection default" — silently blowing the budget far
		// worse than the narrow overrun accepted here.
		asyncMaxConns = 1
	}
	syncMaxConns := cfg.WorkerMaxConns - asyncMaxConns
	if syncMaxConns < 1 {
		syncMaxConns = 1
	}
	workerClient := proxy.New(cfg.WorkerURL, time.Duration(cfg.WorkerTimeoutMS)*time.Millisecond, syncMaxConns, components.Policy).
		WithSecret(cfg.WorkerSharedSecret)
	bucket := ratelimit.New(redisClient)
	plans := billing.NewPlanCache(pool)
	quotaTracker := quota.New(redisClient)
	recorder := usage.NewRecorder(pool, quotaTracker)
	stripe := billing.NewStripeClient(cfg.StripeSecretKey, cfg.StripeMeterName)
	flusher := usage.NewFlusher(pool, stripe, 30*time.Second)
	webhook := billing.NewWebhook(cfg.StripeWebhookSecret, pool)
	respCacheStore := respcache.NewStore(redisClient)
	// Constructed once here and injected into BOTH server.Deps (below) and
	// jobs.NewExecutor: a single shared Emitter/delivery-worker instance, not
	// one per consumer. context.Background() keeps the delivery worker alive
	// for the process lifetime; process exit (SIGTERM/SIGKILL) stops the
	// goroutine, same as every other long-lived background component here.
	emitter := webhookout.NewEmitter(context.Background(), pool)
	jobStore := jobs.NewStore(pool)
	// jobProxyClient is deliberately separate from workerClient: proxy.New's
	// timeout argument becomes http.Client.Timeout, a hard ceiling that
	// governs independently of (and can fire before) any shorter context
	// deadline — it is NOT extended by a longer context.WithTimeout. Reusing
	// workerClient (built above with WORKER_TIMEOUT_MS, the fast synchronous
	// path's budget) would silently cap every async worker call at that
	// ceiling regardless of JOB_TIMEOUT_MS or a longer per-route AsyncRoutes
	// override, defeating the entire point of the async path for exactly the
	// long-running products it exists to serve. Sized to the largest
	// possible per-job deadline so the job's own context timeout — not this
	// client's — is always what actually governs.
	jobHTTPTimeout := cfg.JobTimeout()
	for _, secs := range server.AsyncRoutes {
		if d := time.Duration(secs) * time.Second; d > jobHTTPTimeout {
			jobHTTPTimeout = d
		}
	}
	jobProxyClient := proxy.New(cfg.WorkerURL, jobHTTPTimeout, asyncMaxConns, components.Policy).
		WithSecret(cfg.WorkerSharedSecret)
	jobExecutor := jobs.NewExecutor(jobStore, jobProxyClient, recorder, jobs.ExecutorConfig{
		PoolSize:      cfg.JobWorkerPoolSize,
		PollInterval:  cfg.JobPollInterval(),
		JobTimeout:    cfg.JobTimeout(),
		ErrorExposure: cfg.ErrorExposure,
	})
	// Same Emitter instance injected into server.Deps below — one shared
	// delivery worker, not a second Emitter.
	jobExecutor.SetEmitter(emitter)
	reaper := jobs.NewReaper(pool, cfg.JobRetention(), cfg.JobReaperInterval())
	idempReaper := idempotency.NewReaper(pool, cfg.IdempotencyRetention(), cfg.IdempotencyReaperInterval())
	deliveryReaper := webhookout.NewDeliveryReaper(pool, cfg.WebhookDeliveryRetention(), cfg.WebhookDeliveryReaperInterval())

	// Async: flush usage to Stripe.
	go flusher.Run(rootCtx)

	// Async: delete terminal async_jobs rows past JOB_RETENTION_DAYS. Inert
	// (no-op) until an operator opts in, see jobs.Reaper.Run.
	go reaper.Run(rootCtx)

	// Async: delete old idempotency_keys rows past IDEMPOTENCY_RETENTION_DAYS.
	// Inert (no-op) until an operator opts in, see idempotency.Reaper.Run.
	go idempReaper.Run(rootCtx)

	// Async: delete delivered webhook_deliveries rows past
	// WEBHOOK_DELIVERY_RETENTION_DAYS. dead_letter rows are never deleted.
	// Inert (no-op) until an operator opts in, see webhookout.DeliveryReaper.Run.
	go deliveryReaper.Run(rootCtx)

	// Async: execute durable jobs opted into routes_table.go's AsyncRoutes.
	// jobsDone closes once jobExecutor.Run has released any jobs it still
	// held claimed and returned — the shutdown sequence below waits on it
	// so a process exit never races the release ("no lost work").
	jobsDone := make(chan struct{})
	go func() {
		defer close(jobsDone)
		jobExecutor.Run(rootCtx)
	}()

	srv := &http.Server{
		Addr: fmt.Sprintf(":%d", cfg.Port),
		Handler: server.NewRouter(&server.Deps{
			Cfg:            cfg,
			Proxy:          workerClient,
			Auth:           authStore,
			Bucket:         bucket,
			Plans:          plans,
			Recorder:       recorder,
			Webhook:        webhook,
			Quota:          quotaTracker,
			Redis:          &redisPinger{redisClient},
			DB:             pool,
			Emitter:        emitter,
			RespCache:      respCacheStore,
			PG:             &pgPinger{pool},
			TracerProvider: components.TracerProvider,
			OperatorStore:  operator.NewStore(pool),
			OperatorToken:  cfg.OperatorToken,
		}),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Separate /metrics listener on METRICS_PORT — keeps Prometheus off the public API surface.
	metricsMux := http.NewServeMux()
	metricsMux.Handle("/metrics", observability.Handler())
	metricsSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.MetricsPort),
		Handler:           metricsMux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		log.Info().Int("port", cfg.Port).Str("worker", cfg.WorkerURL).Msg("gateway listening")
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal().Err(err).Msg("server failed")
		}
	}()

	go func() {
		log.Info().Int("port", cfg.MetricsPort).Msg("metrics listening")
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Warn().Err(err).Msg("metrics server stopped")
		}
	}()

	<-rootCtx.Done()
	stop()
	log.Info().Msg("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("graceful shutdown failed")
	}
	_ = metricsSrv.Shutdown(shutdownCtx)
	// rootCtx is already cancelled (signal.NotifyContext, above), so
	// jobExecutor.Run is already draining in-flight jobs and releasing any it
	// still holds claimed back to 'queued'. Wait for it within the shutdown
	// budget so process exit cannot race that release.
	select {
	case <-jobsDone:
	case <-shutdownCtx.Done():
		log.Warn().Msg("job executor did not stop within shutdown budget")
	}
	if err := components.Shutdown(shutdownCtx); err != nil {
		log.Warn().Err(err).Msg("runtime shutdown failed")
	}
	authStore.Close()
}
