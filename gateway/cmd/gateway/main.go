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

	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/cache"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/ratelimit"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
	"github.com/Unluckyathecking/crucible/gateway/internal/tracing"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
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

	authStore := auth.NewStore(pool, redisClient, cfg.APIKeyHashSalt)
	workerClient := proxy.New(cfg.WorkerURL, time.Duration(cfg.WorkerTimeoutMS)*time.Millisecond, cfg.WorkerMaxConns)
	bucket := ratelimit.New(redisClient)
	plans := billing.NewPlanCache(pool)
	quotaTracker := quota.New(redisClient)
	recorder := usage.NewRecorder(pool, quotaTracker)
	stripe := billing.NewStripeClient(cfg.StripeSecretKey, cfg.StripeMeterName)
	flusher := usage.NewFlusher(pool, stripe, 30*time.Second)
	webhook := billing.NewWebhook(cfg.StripeWebhookSecret, pool)

	// Async: flush usage to Stripe.
	go flusher.Run(rootCtx)

	// Tracing — disabled by default; enabled when OTEL_TRACING_ENABLED=true.
	var tracerProvider oteltrace.TracerProvider
	if cfg.OtelTracingEnabled {
		tp, shutdown, err := tracing.NewProvider(rootCtx, cfg.OtelExporterEndpoint, cfg.OtelExporterInsecure, cfg.OtelSampleRatio)
		if err != nil {
			log.Fatal().Err(err).Msg("tracing provider init failed")
		}
		defer func() { _ = shutdown(context.Background()) }()
		tracerProvider = tp
	}

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
			PG:             &pgPinger{pool},
			TracerProvider: tracerProvider,
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
	authStore.Close()
}
