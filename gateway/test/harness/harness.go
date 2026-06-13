// Package harness provides NewGatewayTestServer: a test helper that boots the
// full gateway middleware chain against real Postgres and Redis with an
// in-process worker stub. DSN and RedisURL are required; callers set Options
// fields as needed. Migrations are applied automatically once per test process
// via sync.Once.
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
func TestSalt() string { return testSalt }

const (
	// TestAPIKeyPrefix is the key prefix configured in every harness server.
	TestAPIKeyPrefix = "cru_"

	defaultWorkerTimeoutMS = 5000
	defaultProxyPoolSize   = 8
	defaultBodyLimitBytes  = 1 << 20
	defaultDBPoolSize      = 5

	serverBootTimeout         = 30 * time.Second
	cleanupTimeout            = 60 * time.Second // budget for customer cleanup including retry loop
	maxCleanupRetries         = 3
	cleanupRetryTimeout       = 10 * time.Second
	planExistenceCheckTimeout = 5 * time.Second  // plan lookup before customer insert

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

// routesMu serializes replacement of server.V1Routes during server.NewRouter calls,
// preventing concurrent test goroutines from replacing the route table while another
// is reading it or calling NewRouter.
var routesMu sync.Mutex

// migrateOnce runs migrations exactly once per test process for speed.
// If the first attempt fails, migrateOnceErr remains set and all subsequent
// tests in the same process fail; callers must ensure Postgres is ready before
// running tests. Each migration file is idempotent: for example,
// 0013_error_events_fk_noaction.sql uses a PL/pgSQL guard to skip the repair
// work when the FK is already valid, so re-runs are cheap no-ops.
var (
	migrateOnce    sync.Once
	migrateOnceErr error
)

// Options configures a gateway test server.
type Options struct {
	// Routes overrides server.V1Routes. Nil means use production routes.
	// Non-nil callers must not use t.Parallel.
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
		t.Fatalf("harness: open postgres: %v", err)
	}
	// Register pool.Close first so it runs last in LIFO (pool stays open throughout cleanup).
	// Register workerSrv.Close immediately after so the worker shuts down before pool closes.
	t.Cleanup(pool.Close)
	t.Cleanup(workerSrv.Close)
	migrateOnce.Do(func() {
		applyCtx, applyCancel := context.WithTimeout(context.Background(), serverBootTimeout)
		migrateOnceErr = db.Apply(applyCtx, pool)
		applyCancel()
	})
	if migrateOnceErr != nil {
		t.Fatalf("harness: apply migrations: %v", migrateOnceErr)
	}

	redisCtx, redisCancel := context.WithTimeout(context.Background(), serverBootTimeout)
	defer redisCancel()
	rdb, err := cache.NewRedis(redisCtx, opts.RedisURL)
	if err != nil {
		t.Fatalf("harness: open redis: %v", err)
	}

	cfg := &config.Config{
		BodyLimitBytes:  defaultBodyLimitBytes,
		DashboardOrigin: "http://localhost:3001",
		ErrorExposure:   "full",
		APIKeyPrefix:    TestAPIKeyPrefix,
		APIKeyHashSalt:  testSalt,
	}

	authStore := auth.NewStore(pool, rdb, testSalt)
	// Single cleanup closes authStore first (draining the background last_used_at
	// goroutine while Redis is still open), then rdb via defer so rdb.Close runs
	// even if authStore.Close panics. pool.Close is registered separately and earlier
	// so it runs last in LIFO order, remaining open throughout.
	t.Cleanup(func() {
		defer func() {
			if err := rdb.Close(); err != nil {
				t.Logf("harness: redis close: %v", err)
			}
		}()
		authStore.Close() // Close() drains the background goroutine; returns no error
	})

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
		// Shallow copy is safe: RouteDescriptor structs are value types and *Schema
		// pointers are read-only once registered.
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
// the plan to its pre-test state.
func (ts *TestServer) CreatePlan(t *testing.T, id string, ratePerMinute int64, monthlyCap int64) {
	t.Helper()
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
	ctx := context.Background()

	var (
		prevRate int64
		prevCap  *int64 // nil = NULL (unlimited)
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

	var capPtr *int64
	if monthlyCap > 0 {
		capPtr = &monthlyCap
	}
	if _, err := ts.DB.Exec(ctx, `
		INSERT INTO plans (id, display_name, rate_limit_per_minute, monthly_unit_cap)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		  SET display_name          = EXCLUDED.display_name,
		      rate_limit_per_minute = EXCLUDED.rate_limit_per_minute,
		      monthly_unit_cap      = EXCLUDED.monthly_unit_cap
	`, id, testPlanDisplayNamePrefix+id, ratePerMinute, capPtr); err != nil {
		t.Fatalf("harness: create plan %q: %v", id, err)
	}

	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		if existed {
			if _, err := ts.DB.Exec(cctx,
				`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = $3, display_name = $4 WHERE id = $1`,
				id, prevRate, prevCap, prevName,
			); err != nil {
				t.Errorf("harness: restore plan %q: %v", id, err)
				return
			}
		} else {
			if _, err := ts.DB.Exec(cctx, `DELETE FROM plans WHERE id = $1`, id); err != nil {
				t.Errorf("harness: cleanup plan %q: %v", id, err)
				return
			}
		}
	})
}

// CreateCustomer inserts a customer on planID, generates and persists an API key,
// and returns (customerID, rawAPIKey). t.Cleanup removes all rows and Redis keys.
func (ts *TestServer) CreateCustomer(t *testing.T, email, planID string) (uuid.UUID, string) {
	t.Helper()
	if ts.DB == nil {
		t.Fatal("harness: CreateCustomer called on nil TestServer.DB")
	}
	if email == "" {
		t.Fatal("harness: CreateCustomer email must be non-empty")
	}
	if planID == "" {
		t.Fatal("harness: CreateCustomer planID must be non-empty")
	}
	now := time.Now().UTC()
	// Capture both the current and next month now so cleanup never needs to call time.Now()
	// — avoids a mismatch if cleanup runs across a UTC month boundary.
	createdMonth := now.Format("2006-01")
	nextMonth := now.AddDate(0, 1, 0).Format("2006-01")
	// Generate the key and register t.Cleanup before any operations that can t.Fatal.
	// The closure guards customerID == uuid.Nil so nothing is cleaned up if setup
	// fatals before the first INSERT.
	full, prefix, err := auth.Generate(TestAPIKeyPrefix)
	if err != nil {
		t.Fatalf("harness: generate api key: %v", err)
	}
	if prefix == "" {
		t.Fatal("harness: auth.Generate returned empty prefix")
	}
	// customerID == uuid.Nil signals setup fataled before the first INSERT.
	var customerID uuid.UUID

	t.Cleanup(func() {
		if customerID == uuid.Nil {
			return // setup fataled before first insert; nothing to clean up
		}
		cctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
		defer cancel()
		// Redis keys are always cleaned up on function exit, regardless of whether DB
		// cleanup succeeds. A fresh context is used so DB timeout exhaustion cannot
		// cancel the Redis DEL. DEL on non-existent keys returns 0 (not an error).
		defer func() {
			if ts.Redis == nil {
				return
			}
			rctx, rcancel := context.WithTimeout(context.Background(), cleanupRetryTimeout)
			defer rcancel()
			// Key formats mirror the production packages (verified against source):
			//   quota.Tracker: "quota:<customerID>:<YYYY-MM>"  (internal/quota/tracker.go)
			//   ratelimit.Bucket: "rl:<customerID>"            (internal/ratelimit/bucket.go)
			//   auth.Store: "auth:<prefix>"                    (internal/auth/store.go)
			quotaKey := "quota:" + customerID.String() + ":" + createdMonth
			nextQuotaKey := "quota:" + customerID.String() + ":" + nextMonth
			rlKey := "rl:" + customerID.String()
			authKey := "auth:" + prefix
			if delErr := ts.Redis.Del(rctx, quotaKey, nextQuotaKey, rlKey, authKey).Err(); delErr != nil {
				t.Logf("harness: redis cleanup for customer %s: %v", customerID, delErr)
			}
		}()
		cleanupErr := func(table string, opErr error) {
			if opErr == nil {
				return
			}
			// Context cancellation/deadline means the cleanup budget expired; log but don't fail.
			if errors.Is(opErr, context.Canceled) || errors.Is(opErr, context.DeadlineExceeded) {
				t.Logf("harness: cleanup timeout %s for customer %s: %v", table, customerID, opErr)
				return
			}
			var pgErr *pgconn.PgError
			if errors.As(opErr, &pgErr) && pgErr.Code == "23503" {
				t.Errorf("harness: cleanup FK violation %s for customer %s (constraint: %s): %v", table, customerID, pgErr.ConstraintName, opErr)
				return
			}
			t.Errorf("harness: cleanup %s for customer %s: %v", table, customerID, opErr)
		}
		// Delete children before parents (error_events.api_key_id REFERENCES api_keys ON DELETE NO ACTION).
		var err error
		_, err = ts.DB.Exec(cctx, `DELETE FROM usage_events WHERE customer_id = $1`, customerID)
		cleanupErr("usage_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM idempotency_keys WHERE customer_id = $1`, customerID)
		cleanupErr("idempotency_keys", err)
		_, err = ts.DB.Exec(cctx, errorEventsDeleteSQL, customerID)
		cleanupErr("error_events", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_deliveries WHERE endpoint_id IN (SELECT id FROM webhook_endpoints WHERE customer_id = $1)`, customerID)
		cleanupErr("webhook_deliveries", err)
		_, err = ts.DB.Exec(cctx, `DELETE FROM webhook_endpoints WHERE customer_id = $1`, customerID)
		cleanupErr("webhook_endpoints", err)
		// Retry deleting api_keys: the async errorlog goroutine (2s timeout) may insert an
		// error_events row after the DELETE above, causing a transient FK violation.
		// A short backoff between retries lets the async writer finish before retrying.
		var finalKeyErr error
	retryLoop:
		for attempt := 1; attempt <= maxCleanupRetries; attempt++ {
			if cctx.Err() != nil {
				finalKeyErr = cctx.Err()
				break retryLoop
			}
			if attempt > 1 {
				select {
				case <-time.After(50 * time.Millisecond):
				case <-cctx.Done():
					finalKeyErr = cctx.Err()
					break retryLoop // break retryLoop exits the for, not just the select
				}
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
				_, delErr := ts.DB.Exec(fixCtx, errorEventsDeleteSQL, customerID)
				fixCancel()
				if delErr != nil {
					t.Logf("harness: cleanup error_events retry for customer %s: %v", customerID, delErr)
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
		_, err = ts.DB.Exec(cctx, `DELETE FROM customers WHERE id = $1`, customerID)
		cleanupErr("customers", err)
	})

	parsedAddr, err := mail.ParseAddress(email)
	if err != nil {
		t.Fatalf("harness: CreateCustomer email %q is not a valid RFC 5322 address: %v", email, err)
	}
	// Existence check: SELECT 1 + Scan(new(int)) is the idiomatic pgx check.
	planCtx, planCancel := context.WithTimeout(context.Background(), planExistenceCheckTimeout)
	defer planCancel()
	var dummy int
	err = ts.DB.QueryRow(planCtx,
		`SELECT 1 FROM plans WHERE id = $1`, planID,
	).Scan(&dummy)
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("harness: CreateCustomer planID %q does not exist", planID)
	}
	if err != nil {
		t.Fatalf("harness: CreateCustomer planID %q lookup failed: %v", planID, err)
	}

	customerID = uuid.New()
	insertCtx, insertCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer insertCancel()
	_, err = ts.DB.Exec(insertCtx,
		`INSERT INTO customers (id, email, plan_id) VALUES ($1, $2, $3)`,
		customerID, parsedAddr.Address, planID,
	)
	if err != nil {
		t.Fatalf("harness: insert customer %s: %v", customerID, err)
	}

	hash := auth.Hash(testSalt, full)
	_, err = ts.DB.Exec(insertCtx,
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
