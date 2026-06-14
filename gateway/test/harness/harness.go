// Package harness provides NewGatewayTestServer: a test helper that boots the
// full gateway middleware chain against real Postgres and Redis with an
// in-process worker stub. DSN and RedisURL are required; callers set Options
// fields as needed. Migrations are applied automatically once per test process
// via a mutex-guarded idempotent check (not sync.Once; see runMigrations).
package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/Unluckyathecking/crucible/gateway/internal/auth"
	"github.com/Unluckyathecking/crucible/gateway/internal/billing"
	"github.com/Unluckyathecking/crucible/gateway/internal/cache"
	"github.com/Unluckyathecking/crucible/gateway/internal/config"
	"github.com/Unluckyathecking/crucible/gateway/internal/db"
	"github.com/Unluckyathecking/crucible/gateway/internal/errorlog"
	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/gateway/internal/proxy"
	"github.com/Unluckyathecking/crucible/gateway/internal/quota"
	"github.com/Unluckyathecking/crucible/gateway/internal/ratelimit"
	"github.com/Unluckyathecking/crucible/gateway/internal/server"
	"github.com/Unluckyathecking/crucible/gateway/internal/usage"
)

var testSalt string

// TestSalt returns the per-process API key hash salt used by all harness instances.
// The salt is immutable after init() and shared across all parallel tests because
// auth.Store is process-scoped; callers must not modify it.
func TestSalt() string { return testSalt }

const (
	// TestAPIKeyPrefix is the key prefix configured in every harness server.
	TestAPIKeyPrefix = "cru_"

	defaultWorkerTimeoutMS = 5000
	defaultProxyPoolSize   = 8
	defaultBodyLimitBytes  = 1 << 20
	defaultDBPoolSize      = 5

	serverBootTimeout         = 30 * time.Second
	cleanupTimeout            = 60 * time.Second      // budget for customer cleanup including retry loop
	maxCleanupRetries         = 3
	cleanupRetryTimeout       = 10 * time.Second      // timeout for each api_keys DELETE retry attempt
	redisCleanupTimeout       = 10 * time.Second      // timeout for the Redis DEL in customer cleanup
	cleanupRetryBackoff       = 50 * time.Millisecond // delay between api_keys delete retries
	planExistenceCheckTimeout = 5 * time.Second       // plan lookup before customer insert
	customerInsertTimeout     = 10 * time.Second      // customer + api_key INSERT in CreateCustomer

	// testPlanDisplayNamePrefix is prepended to the plan ID to form the display name in CreatePlan.
	testPlanDisplayNamePrefix = "Test Plan "

	// errorEventsDeleteSQL removes all error_events rows for a customer.
	// error_events.customer_id is NOT NULL and indexed via idx_error_events_customer_created,
	// so this is a fast indexed delete.
	errorEventsDeleteSQL = `DELETE FROM error_events WHERE customer_id = $1`
)

func init() {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("harness: failed to generate test salt: " + err.Error())
	}
	testSalt = hex.EncodeToString(b)
}

// routesMu serializes replacement of server.V1Routes during server.NewRouter calls.
// Tests that set opts.Routes may use t.Parallel; routesMu ensures exclusive access
// during the swap-and-restore so no goroutine observes intermediate route state.
var routesMu sync.Mutex

// migrateOnce runs migrations exactly once per test process for speed.
// Unlike sync.Once, we use a mutex + bool + error so that the error from the
// first (and only) attempt is propagated to all subsequent callers. sync.Once
// has no mechanism to expose the result of the wrapped function. If the first
// attempt fails, migrateErr remains set and all later tests in the same process
// fail fast. Callers must ensure Postgres is ready before running tests.
// Migration files in this project are individually idempotent
// (CREATE IF NOT EXISTS / ON CONFLICT DO NOTHING / PL/pgSQL guards), so repeated
// runs against the same schema are safe.
// All concurrent callers in the same process must target the same Postgres
// schema; do not use this harness from multiple packages in the same go test
// invocation unless they share the same DSN.
var (
	migrateMu   sync.Mutex
	migrateDone bool
	migrateErr  error
)

// runMigrations applies schema migrations against pool exactly once per test
// process. The mutex is held for the full call: on the first invocation it
// serialises the migration; on subsequent calls migrateDone is already true
// so the locked section is near-zero and returns immediately.
func runMigrations(pool *pgxpool.Pool) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	if !migrateDone {
		ctx, cancel := context.WithTimeout(context.Background(), serverBootTimeout)
		defer cancel()
		migrateErr = db.Apply(ctx, pool)
		if migrateErr == nil {
			migrateDone = true
		}
	}
	return migrateErr
}

// Options configures a gateway test server.
type Options struct {
	// Routes overrides server.V1Routes. Nil means use production routes.
	// Non-nil callers may use t.Parallel; routesMu serializes the swap.
	Routes []openapi.RouteDescriptor

	// WorkerHandler handles POST /invoke calls forwarded by the gateway proxy.
	WorkerHandler http.Handler

	// DSN is a real Postgres connection string.
	DSN string

	// RedisURL is a real Redis connection string.
	RedisURL string

	// WorkerTimeoutMS caps the gateway→worker call. 0 means use the default (5000 ms).
	WorkerTimeoutMS int
}

// TestServer is a running gateway backed by real Postgres and Redis.
type TestServer struct {
	// Server is the gateway httptest.Server.
	Server *httptest.Server
	// Worker is the in-process worker httptest.Server.
	Worker *httptest.Server
	// DB gives direct Postgres access for assertion queries.
	DB *pgxpool.Pool
	// Redis gives direct Redis access for assertion queries.
	Redis *redis.Client
}

// NewGatewayTestServer boots the full gateway middleware chain via server.NewRouter
// against real Postgres and Redis and returns the started test server.
func NewGatewayTestServer(t *testing.T, opts Options) *TestServer {
	t.Helper()
	if opts.WorkerHandler == nil {
		t.Fatal("harness: WorkerHandler is required")
	}
	if opts.DSN == "" {
		t.Fatal("harness: DSN is required")
	}
	if dsnURL, err := url.Parse(opts.DSN); err != nil || (dsnURL.Scheme != "postgres" && dsnURL.Scheme != "postgresql") || dsnURL.Host == "" {
		t.Fatalf("harness: DSN must be a valid postgres:// or postgresql:// URL, got: %s", opts.DSN)
	}
	if opts.RedisURL == "" {
		t.Fatal("harness: RedisURL is required")
	}
	if redisURL, err := url.Parse(opts.RedisURL); err != nil || (redisURL.Scheme != "redis" && redisURL.Scheme != "rediss") || redisURL.Host == "" {
		t.Fatalf("harness: RedisURL must be a valid redis:// or rediss:// URL, got: %s", opts.RedisURL)
	}

	if opts.WorkerTimeoutMS < 0 {
		t.Fatalf("harness: WorkerTimeoutMS must be >= 0 (use 0 for default %d ms), got: %d", defaultWorkerTimeoutMS, opts.WorkerTimeoutMS)
	}
	if opts.WorkerTimeoutMS == 0 {
		opts.WorkerTimeoutMS = defaultWorkerTimeoutMS
	}
	const maxWorkerTimeoutMS = 300_000 // 5 min; matches production gateway proxy max, prevents accidentally huge values
	if opts.WorkerTimeoutMS > maxWorkerTimeoutMS {
		t.Fatalf("harness: WorkerTimeoutMS %d exceeds maximum %d ms", opts.WorkerTimeoutMS, maxWorkerTimeoutMS)
	}

	workerSrv := httptest.NewServer(opts.WorkerHandler)

	poolCtx, poolCancel := context.WithTimeout(context.Background(), serverBootTimeout)
	defer poolCancel()
	pool, err := db.NewPool(poolCtx, opts.DSN, defaultDBPoolSize)
	if err != nil {
		workerSrv.Close() // no t.Cleanup registered yet; close manually before failing
		t.Fatalf("harness: open postgres: %v", err)
	}
	// LIFO ordering is intentional: workerSrv.Close (registered last) runs first so
	// the worker can drain any in-flight proxy requests while Postgres is still open.
	// The pool-failure error path on line above closes workerSrv manually before
	// t.Fatalf, so both resources are cleaned up even on boot failures.
	t.Cleanup(pool.Close)
	t.Cleanup(workerSrv.Close)
	if err := runMigrations(pool); err != nil {
		t.Fatalf("harness: apply migrations: %v", err)
	}

	redisCtx, redisCancel := context.WithTimeout(context.Background(), serverBootTimeout)
	defer redisCancel()
	rdb, err := cache.NewRedis(redisCtx, opts.RedisURL)
	if err != nil {
		t.Fatalf("harness: open redis: %v", err)
	}
	// Registered before authStore cleanup so LIFO runs rdb.Close after authStore.Close,
	// keeping Redis open while authStore drains its background goroutine.
	t.Cleanup(func() {
		if err := rdb.Close(); err != nil {
			t.Logf("harness: redis close: %v", err)
		}
	})

	cfg := &config.Config{
		BodyLimitBytes:  defaultBodyLimitBytes,
		DashboardOrigin: "http://localhost:3001",
		ErrorExposure:   "full",
		APIKeyPrefix:    TestAPIKeyPrefix,
		APIKeyHashSalt:  testSalt,
	}

	authStore := auth.NewStore(pool, rdb, testSalt)
	// auth.Store.Close() signature: func (s *Store) Close() — no error return.
	// Registered after rdb cleanup so LIFO runs this first, draining the background
	// last_used_at goroutine while Redis is still open.
	t.Cleanup(authStore.Close)

	// proxy.Client has no Close() method; its http.Transport closes idle connections
	// automatically when workerSrv is shut down and the IdleConnTimeout (90 s) elapses.
	workerClient := proxy.New(
		workerSrv.URL,
		time.Duration(opts.WorkerTimeoutMS)*time.Millisecond,
		defaultProxyPoolSize,
	)

	bucket := ratelimit.New(rdb)
	plans := billing.NewPlanCache(pool)
	quotaTracker := quota.New(rdb)
	recorder := usage.NewRecorder(pool, quotaTracker)
	// dummy secret: no real Stripe webhook calls in tests; prefix differs per process.
	webhook := billing.NewWebhook("whsec_test_"+uuid.New().String(), pool)

	deps := &server.Deps{
		Cfg:           cfg,
		Proxy:         workerClient,
		Auth:          authStore,
		Bucket:        bucket,
		Plans:         plans,
		Recorder:      recorder,
		Webhook:       webhook,
		Quota:         quotaTracker,
		Redis:         &redisPinger{rdb},
		PG:            &pgPinger{pool},
		DB:            pool,
		ErrorRecorder: errorlog.New(pool),
	}

	// Validate before acquiring the lock; t.Fatal here never holds routesMu.
	if opts.Routes != nil && len(opts.Routes) == 0 {
		t.Fatal("harness: Routes must be non-empty; use nil for production routes")
	}
	// routesMu serializes mutation of server.V1Routes and holds the lock during
	// server.NewRouter so the router reads a consistent, fully-replaced route table.
	// The original slice is restored in defer before unlock, so no subsequent caller
	// observes test-specific routes after NewRouter returns.
	// Narrow the lock scope to just the V1Routes mutation and NewRouter call so
	// routesMu is released before httptest.NewServer (which doesn't access routes).
	// The IIFE's defer restores V1Routes and unlocks even if NewRouter panics.
	handler := func() http.Handler {
		routesMu.Lock()
		// Deep copy of slice header (new backing array). RouteDescriptor structs are
		// value types and *Schema pointers are read-only once registered, so copying
		// the slice elements is sufficient to protect server.V1Routes from mutation.
		orig := append([]openapi.RouteDescriptor(nil), server.V1Routes...)
		defer func() {
			server.V1Routes = orig
			routesMu.Unlock()
		}()
		if opts.Routes != nil {
			server.V1Routes = append([]openapi.RouteDescriptor(nil), opts.Routes...)
		}
		return server.NewRouter(deps)
	}()

	gw := httptest.NewServer(handler)
	t.Cleanup(gw.Close)

	return &TestServer{
		Server: gw,
		Worker: workerSrv,
		DB:     pool,
		Redis:  rdb,
	}
}

// CreatePlan inserts or updates a plan row. ratePerMinute=0 means unlimited;
// monthlyCap=0 means unlimited (stored as NULL). Registers t.Cleanup to restore
// the plan to its pre-test state. Plan IDs must be unique across concurrent tests
// (t.Parallel()) because the plan table is shared DB state; callers are responsible
// for choosing non-colliding IDs.
func (ts *TestServer) CreatePlan(t *testing.T, id string, ratePerMinute int64, monthlyCap int64) {
	t.Helper()
	if ts == nil {
		t.Fatal("harness: CreatePlan called on nil TestServer")
	}
	if ts.DB == nil {
		t.Fatal("harness: CreatePlan called on nil TestServer.DB")
	}
	if id == "" {
		t.Fatal("harness: CreatePlan id must be non-empty")
	}
	if ratePerMinute < 0 {
		t.Fatal("harness: CreatePlan ratePerMinute must be >= 0 (use 0 for unlimited)")
	}
	if monthlyCap < 0 {
		t.Fatal("harness: CreatePlan monthlyCap must be >= 0 (use 0 for unlimited)")
	}
	// A t.Cleanup registered at the end of this function restores the plan to its
	// pre-call state (or deletes it if it did not exist before) after the test ends.
	ctx := context.Background()

	var (
		prevRate int64
		prevCap  pgtype.Int8 // Valid=false when monthly_unit_cap is NULL (unlimited)
		prevName string
		existed  bool
	)
	if err := ts.DB.QueryRow(ctx,
		`SELECT rate_limit_per_minute, monthly_unit_cap, display_name FROM plans WHERE id = $1`, id,
	).Scan(&prevRate, &prevCap, &prevName); err == nil {
		existed = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("harness: snapshot plan %q: %v", id, err)
	}

	var capArg *int64
	if monthlyCap > 0 {
		capArg = &monthlyCap
	}
	if _, err := ts.DB.Exec(ctx, `
		INSERT INTO plans (id, display_name, rate_limit_per_minute, monthly_unit_cap)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		  SET display_name          = EXCLUDED.display_name,
		      rate_limit_per_minute = EXCLUDED.rate_limit_per_minute,
		      monthly_unit_cap      = EXCLUDED.monthly_unit_cap
	`, id, testPlanDisplayNamePrefix+id, ratePerMinute, capArg); err != nil {
		t.Fatalf("harness: create plan %q: %v", id, err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if existed {
			// Use two separate queries so monthly_unit_cap is expressed as SQL NULL
			// when the plan had no cap, rather than relying on driver NULL coercion.
			var err error
			if prevCap.Valid {
				_, err = ts.DB.Exec(cctx,
					`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = $3, display_name = $4 WHERE id = $1`,
					id, prevRate, prevCap.Int64, prevName,
				)
			} else {
				_, err = ts.DB.Exec(cctx,
					`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = NULL, display_name = $3 WHERE id = $1`,
					id, prevRate, prevName,
				)
			}
			if err != nil {
				t.Logf("harness: restore plan %q: %v", id, err)
			}
		} else {
			if _, err := ts.DB.Exec(cctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
				t.Logf("harness: cleanup plan %q: %v", id, err)
			}
		}
	})
}

// CreateCustomer inserts a customer on planID, generates and persists an API key,
// and returns (customerID, rawAPIKey). t.Cleanup removes all rows and Redis keys.
func (ts *TestServer) CreateCustomer(t *testing.T, email, planID string) (uuid.UUID, string) {
	t.Helper()
	if ts == nil {
		t.Fatal("harness: CreateCustomer called on nil TestServer")
	}
	if ts.DB == nil {
		t.Fatal("harness: CreateCustomer called on nil TestServer.DB")
	}
	if ts.Redis == nil {
		t.Fatal("harness: CreateCustomer called on nil TestServer.Redis")
	}
	if email == "" {
		t.Fatal("harness: CreateCustomer email must be non-empty")
	}
	if planID == "" {
		t.Fatal("harness: CreateCustomer planID must be non-empty")
	}
	_, err := mail.ParseAddress(email)
	if err != nil {
		t.Fatalf("harness: CreateCustomer email %q is not a valid RFC 5322 address: %v", email, err)
	}

	// Validate planID before generating a key so we don't discard a key immediately
	// on an invalid planID — auth.Generate is a cheap in-memory op but the intent
	// is clearer when it follows the guard that justifies proceeding.
	planCtx, planCancel := context.WithTimeout(context.Background(), planExistenceCheckTimeout)
	defer planCancel()
	var dummy int
	planErr := ts.DB.QueryRow(planCtx,
		`SELECT 1 FROM plans WHERE id = $1`, planID,
	).Scan(&dummy)
	if errors.Is(planErr, pgx.ErrNoRows) {
		t.Fatalf("harness: CreateCustomer planID %q does not exist", planID)
	}
	if planErr != nil {
		t.Fatalf("harness: CreateCustomer planID %q lookup failed: %v", planID, planErr)
	}

	full, prefix, err := auth.Generate(TestAPIKeyPrefix)
	if err != nil {
		t.Fatalf("harness: generate api key: %v", err)
	}
	if full == "" || prefix == "" {
		t.Fatal("harness: auth.Generate returned empty key or prefix")
	}
	customerID := uuid.New()

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		// Redis keys are always cleaned up on function exit, regardless of whether DB
		// cleanup succeeds. A fresh context is used so DB timeout exhaustion cannot
		// cancel the Redis DEL. DEL on non-existent keys returns 0 (not an error).
		defer func() {
			rctx, rcancel := context.WithTimeout(context.Background(), redisCleanupTimeout)
			defer rcancel()
			// Months computed at cleanup time so the quota keys match what
			// quota.Tracker wrote at request time, even if cleanup runs across
			// a UTC month boundary. Both current and next month are deleted to
			// cover requests that landed just before a boundary.
			// Key formats mirror the production packages (verified against source):
			//   quota.Tracker: "quota:<customerID>:<YYYY-MM>"  (internal/quota/tracker.go)
			//   ratelimit.Bucket: "rl:<customerID>"            (internal/ratelimit/bucket.go)
			//   auth.Store: "auth:<prefix>"                    (internal/auth/store.go)
			now := time.Now().UTC()
			cid := customerID.String()
			month := now.Format("2006-01")
			nextMonth := now.AddDate(0, 1, 0).Format("2006-01")
			if delErr := ts.Redis.Del(rctx,
				"quota:"+cid+":"+month,
				"quota:"+cid+":"+nextMonth,
				"rl:"+cid,
				"auth:"+prefix,
			).Err(); delErr != nil {
				t.Logf("harness: redis cleanup for customer %s: %v", cid, delErr)
			}
		}()
		cleanupErr := func(table string, opErr error) {
			t.Helper()
			if opErr == nil {
				return
			}
			if ts == nil || ts.DB == nil {
				t.Logf("harness: cleanup %s skipped: nil TestServer or DB", table)
				return
			}
			// Context cancellation/deadline means the cleanup budget expired; log but don't fail.
			if errors.Is(opErr, context.Canceled) || errors.Is(opErr, context.DeadlineExceeded) {
				t.Logf("harness: cleanup timeout %s for customer %s: %v", table, customerID, opErr)
				return
			}
			var pgErr *pgconn.PgError
			if errors.As(opErr, &pgErr) && pgErr.Code == "23503" {
				t.Logf("harness: cleanup FK violation %s for customer %s (constraint: %s): %v", table, customerID, pgErr.ConstraintName, opErr)
				return
			}
			t.Logf("harness: cleanup %s for customer %s: %v", table, customerID, opErr)
		}
		// Delete children before parents (error_events.api_key_id REFERENCES api_keys ON DELETE NO ACTION).
		// delErr is reused across sequential cleanup calls so each Exec error is passed
		// directly to cleanupErr; the retry block below uses distinct variable names
		// (retryErr, finalKeyErr) to preserve state across loop iterations.
		var delErr error
		_, delErr = ts.DB.Exec(cctx, `DELETE FROM usage_events WHERE customer_id = $1`, customerID)
		cleanupErr("usage_events", delErr)
		_, delErr = ts.DB.Exec(cctx, `DELETE FROM idempotency_keys WHERE customer_id = $1`, customerID)
		cleanupErr("idempotency_keys", delErr)
		_, delErr = ts.DB.Exec(cctx, errorEventsDeleteSQL, customerID)
		cleanupErr("error_events", delErr)
		_, delErr = ts.DB.Exec(cctx, `DELETE FROM webhook_deliveries WHERE endpoint_id IN (SELECT id FROM webhook_endpoints WHERE customer_id = $1)`, customerID)
		cleanupErr("webhook_deliveries", delErr)
		_, delErr = ts.DB.Exec(cctx, `DELETE FROM webhook_endpoints WHERE customer_id = $1`, customerID)
		cleanupErr("webhook_endpoints", delErr)
		// Retry deleting api_keys: the async errorlog goroutine (2s timeout) may insert an
		// error_events row after the DELETE above, causing a transient FK violation.
		// A short backoff between retries lets the async writer finish before retrying.
		//
		// ctxSleep wraps the timed sleep in a closure so that break/continue in the
		// outer for loop (below) operate on the for loop, not on a select statement.
		// If the select were inlined in the for body, break inside select would exit
		// the select, not the for loop, requiring a labelled break to leave the loop.
		// The closure avoids that label while keeping context-cancellation support.
		ctxSleep := func(d time.Duration) bool {
			if cctx.Err() != nil {
				return false
			}
			tmr := time.NewTimer(d)
			defer tmr.Stop()
			select {
			case <-tmr.C:
				return true
			case <-cctx.Done():
				return false
			}
		}
		var finalKeyErr error
		for attempt := 1; attempt <= maxCleanupRetries; attempt++ {
			if cctx.Err() != nil {
				finalKeyErr = cctx.Err()
				break
			}
			if attempt > 1 && !ctxSleep(cleanupRetryBackoff) {
				finalKeyErr = cctx.Err()
				break
			}
			retryCtx, retryCancel := context.WithTimeout(cctx, cleanupRetryTimeout)
			_, retryErr := ts.DB.Exec(retryCtx, `DELETE FROM api_keys WHERE customer_id = $1`, customerID)
			retryCancel()
			if retryErr == nil {
				finalKeyErr = nil
				break
			}
			var pgErr *pgconn.PgError
			if errors.As(retryErr, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == "error_events_api_key_id_fkey" {
				fixCtx, fixCancel := context.WithTimeout(cctx, 5*time.Second)
				_, fixErr := ts.DB.Exec(fixCtx, errorEventsDeleteSQL, customerID)
				fixCancel()
				if fixErr != nil {
					t.Logf("harness: cleanup error_events retry for customer %s: %v", customerID, fixErr)
				}
				if attempt == maxCleanupRetries {
					finalKeyErr = retryErr
				}
				continue
			}
			// Non-transient error; stop retrying.
			finalKeyErr = retryErr
			break
		}
		if finalKeyErr != nil {
			// api_keys rows remain; customers FK (api_keys.customer_id → customers.id)
			// means DELETE customers would fail too. Log and skip.
			t.Logf("harness: cleanup api_keys for customer %s: %v", customerID, finalKeyErr)
			return
		}
		_, delErr = ts.DB.Exec(cctx, `DELETE FROM customers WHERE id = $1`, customerID)
		cleanupErr("customers", delErr)
	})

	custCtx, custCancel := context.WithTimeout(context.Background(), customerInsertTimeout)
	defer custCancel()
	_, err = ts.DB.Exec(custCtx,
		`INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, $3)`,
		customerID, email, planID,
	)
	if err != nil {
		t.Fatalf("harness: insert customer %s: %v", customerID, err)
	}

	hash := auth.Hash(TestSalt(), full)
	keyCtx, keyCancel := context.WithTimeout(context.Background(), customerInsertTimeout)
	defer keyCancel()
	_, err = ts.DB.Exec(keyCtx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, $3)`,
		customerID, prefix, hash,
	)
	if err != nil {
		t.Fatalf("harness: insert api key for customer %s: %v", customerID, err)
	}

	return customerID, full
}

// CountUsageEvents returns the number of usage_events rows for customerID.
func (ts *TestServer) CountUsageEvents(t *testing.T, customerID uuid.UUID) int64 {
	t.Helper()
	if ts == nil {
		t.Fatal("harness: CountUsageEvents called on nil TestServer")
	}
	if ts.DB == nil {
		t.Fatal("harness: CountUsageEvents called on nil TestServer.DB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	err := ts.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM usage_events WHERE customer_id = $1`, customerID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count usage_events: %v", err)
	}
	return n
}

// CountErrorEvents returns the number of error_events rows for customerID.
func (ts *TestServer) CountErrorEvents(t *testing.T, customerID uuid.UUID) int64 {
	t.Helper()
	if ts == nil {
		t.Fatal("harness: CountErrorEvents called on nil TestServer")
	}
	if ts.DB == nil {
		t.Fatal("harness: CountErrorEvents called on nil TestServer.DB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	err := ts.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM error_events WHERE customer_id = $1`, customerID,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count error_events: %v", err)
	}
	return n
}

// HasIdempotencyKey reports whether an idempotency_keys row exists for the given
// customerID and idempotencyKey. Because the table has a UNIQUE constraint on
// (customer_id, idempotency_key), the result is always 0 or 1 — a boolean existence check.
func (ts *TestServer) HasIdempotencyKey(t *testing.T, customerID uuid.UUID, idempotencyKey string) bool {
	t.Helper()
	if ts == nil {
		t.Fatal("harness: HasIdempotencyKey called on nil TestServer")
	}
	if ts.DB == nil {
		t.Fatal("harness: HasIdempotencyKey called on nil TestServer.DB")
	}
	if idempotencyKey == "" {
		t.Fatal("harness: HasIdempotencyKey idempotencyKey must be non-empty")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int64
	err := ts.DB.QueryRow(ctx,
		`SELECT COUNT(*) FROM idempotency_keys WHERE customer_id = $1 AND idempotency_key = $2`,
		customerID, idempotencyKey,
	).Scan(&n)
	if err != nil {
		t.Fatalf("harness: count idempotency_key: %v", err)
	}
	return n == 1
}

// redisPinger adapts *redis.Client to server.HealthChecker.
type redisPinger struct{ c *redis.Client }

func (r *redisPinger) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }

var _ server.HealthChecker = (*redisPinger)(nil)

// pgPinger adapts *pgxpool.Pool to server.HealthChecker.
type pgPinger struct{ p *pgxpool.Pool }

func (p *pgPinger) Ping(ctx context.Context) error { return p.p.Ping(ctx) }

var _ server.HealthChecker = (*pgPinger)(nil)
