// Package harness provides a test helper that boots the full gateway middleware
// chain against real Postgres and Redis with an in-process worker stub.
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
	maxWorkerTimeoutMS     = 300_000 // 5 min; matches production gateway proxy max
	defaultProxyPoolSize   = 8
	defaultBodyLimitBytes  = 1 << 20
	defaultDBPoolSize      = 5

	serverBootTimeout         = 30 * time.Second
	cleanupTimeout            = 60 * time.Second      // budget for customer cleanup including retry loop
	maxCleanupRetries         = 3
	cleanupRetryTimeout       = 10 * time.Second      // timeout for each api_keys DELETE retry attempt
	redisCleanupTimeout       = 10 * time.Second      // timeout for the Redis DEL in customer cleanup
	cleanupRetryBackoff       = 500 * time.Millisecond // delay between api_keys delete retries; allows async errorlog goroutine to finish
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

// logCleanupErr logs a non-nil cleanup error for table/customerID without
// failing the test. Context errors are logged at "timeout" level; FK violations
// are annotated with the constraint name. All other errors are logged verbatim.
func logCleanupErr(t *testing.T, customerID uuid.UUID, table string, opErr error) {
	t.Helper()
	if opErr == nil {
		return
	}
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

// ctxSleep sleeps for d or until ctx is cancelled, returning false if cancelled.
// Defined at package level (not as a closure) to avoid a new allocation per call.
func ctxSleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	tmr := time.NewTimer(d)
	defer func() {
		// Stop the timer and drain the channel if it already fired.
		// time.Timer.Stop() returns false when the timer has already fired;
		// the channel must be drained to prevent a spurious receive by a
		// future caller. The non-blocking select handles the race where
		// Stop returns false but the channel was already consumed by the
		// select below (tmr.C branch).
		if !tmr.Stop() {
			select {
			case <-tmr.C:
			default:
			}
		}
	}()
	select {
	case <-tmr.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// routesMu serializes replacement of server.V1Routes during server.NewRouter calls.
// Tests that set opts.Routes may use t.Parallel; routesMu ensures exclusive access
// during the swap-and-restore so no goroutine observes intermediate route state.
var routesMu sync.Mutex

// migrateMu + migrateDone serialise db.Apply across concurrent NewGatewayTestServer
// calls within a single test binary. This is necessary because Postgres's
// CREATE INDEX IF NOT EXISTS is not atomic under concurrent transactions: two
// goroutines can both observe the index as absent before either commits, then
// both attempt the INSERT into pg_class and one gets a duplicate-key error.
//
// These variables are package-level, not process-global. Go compiles each test
// package into an independent binary, so "go test -p N ./..." runs N separate
// processes each with their own copy of these vars. Parallel package execution
// is therefore unaffected: no two packages share this state.
//
// sync.Mutex + bool is used rather than sync.Once so the locked-but-not-yet-done
// state is explicit and the error is stored separately without relying on the
// closure-capture mechanism that sync.Once requires.
var (
	migrateMu   sync.Mutex
	migrateDone bool
	migrateErr  error
)

// runMigrations applies db.Apply exactly once per test binary, caching the
// result for all subsequent callers. The migrateMu lock is held only while
// migrations are running; once migrateDone is true, readers return immediately.
func runMigrations(pool *pgxpool.Pool) error {
	migrateMu.Lock()
	defer migrateMu.Unlock()
	if migrateDone {
		return migrateErr
	}
	ctx, cancel := context.WithTimeout(context.Background(), serverBootTimeout)
	defer cancel()
	migrateErr = db.Apply(ctx, pool)
	migrateDone = true
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
	if opts.WorkerTimeoutMS > maxWorkerTimeoutMS {
		t.Fatalf("harness: WorkerTimeoutMS %d exceeds maximum %d ms", opts.WorkerTimeoutMS, maxWorkerTimeoutMS)
	}

	// Declare all closeable resources upfront so the single cleanup closure
	// below can guard each with a nil check, regardless of where setup exits.
	var (
		workerSrv *httptest.Server
		pool      *pgxpool.Pool
		rdb       *redis.Client
		authStore *auth.Store
		gw        *httptest.Server
	)
	// Single cleanup registered before any resource is created: closes everything
	// in dependency order (gw → authStore → rdb → pool → workerSrv) even when
	// setup panics or fatals mid-way. Nil checks skip resources never initialised.
	t.Cleanup(func() {
		if gw != nil {
			gw.Close()
		}
		if authStore != nil {
			authStore.Close()
		}
		if rdb != nil {
			if err := rdb.Close(); err != nil {
				t.Errorf("harness: redis close: %v", err)
			}
		}
		if pool != nil {
			pool.Close()
		}
		if workerSrv != nil {
			workerSrv.Close()
		}
	})

	workerSrv = httptest.NewServer(opts.WorkerHandler)

	poolCtx, poolCancel := context.WithTimeout(context.Background(), serverBootTimeout)
	defer poolCancel()
	var err error
	pool, err = db.NewPool(poolCtx, opts.DSN, defaultDBPoolSize)
	if err != nil {
		t.Fatalf("harness: open postgres: %v", err)
	}

	if err := runMigrations(pool); err != nil {
		t.Fatalf("harness: apply migrations: %v", err)
	}

	redisCtx, redisCancel := context.WithTimeout(context.Background(), serverBootTimeout)
	defer redisCancel()
	rdb, err = cache.NewRedis(redisCtx, opts.RedisURL)
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

	// auth.NewStore returns *Store with no error return; it never returns nil.
	authStore = auth.NewStore(pool, rdb, testSalt)

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
	//
	// No data race on V1Routes: once server.NewRouter returns, the resulting
	// handler (a chi router) serves requests without ever re-reading V1Routes.
	// The routes are baked into the chi router tree at construction time.
	// In-flight request goroutines in a running httptest.Server therefore never
	// touch V1Routes, making the mutex protection on NewRouter sufficient.
	//
	// Narrow the lock scope to just the V1Routes mutation and NewRouter call so
	// routesMu is released before httptest.NewServer (which doesn't access routes).
	// The IIFE's defer restores V1Routes and unlocks even if NewRouter panics.
	handler := func() http.Handler {
		routesMu.Lock()
		// Copy slice elements into a new backing array so mutations don't affect
		// server.V1Routes. RouteDescriptor structs are value types and *Schema
		// pointers are read-only once registered, so element-level copy is sufficient.
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

	gw = httptest.NewServer(handler)

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
	// planID is an explicit local copy of the id parameter so the t.Cleanup
	// closure below captures a named variable rather than the parameter slot,
	// making the capture-by-value intent unambiguous to readers and tools.
	planID := id
	// A t.Cleanup registered at the end of this function restores the plan to its
	// pre-call state (or deletes it if it did not exist before) after the test ends.
	ctx, cancel := context.WithTimeout(context.Background(), planExistenceCheckTimeout)
	defer cancel()

	var (
		prevRate int64
		prevCap  pgtype.Int8 // Valid=false when monthly_unit_cap is NULL (unlimited)
		prevName string
		existed  bool
	)
	if err := ts.DB.QueryRow(ctx,
		`SELECT rate_limit_per_minute, monthly_unit_cap, display_name FROM plans WHERE id = $1`, planID,
	).Scan(&prevRate, &prevCap, &prevName); err == nil {
		existed = true
	} else if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("harness: snapshot plan %q: %v", planID, err)
	}

	capArg := pgtype.Int8{}
	if monthlyCap > 0 {
		capArg = pgtype.Int8{Int64: monthlyCap, Valid: true}
	}
	if _, err := ts.DB.Exec(ctx, `
		INSERT INTO plans (id, display_name, rate_limit_per_minute, monthly_unit_cap)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (id) DO UPDATE
		  SET display_name          = EXCLUDED.display_name,
		      rate_limit_per_minute = EXCLUDED.rate_limit_per_minute,
		      monthly_unit_cap      = EXCLUDED.monthly_unit_cap
	`, planID, testPlanDisplayNamePrefix+planID, ratePerMinute, capArg); err != nil {
		t.Fatalf("harness: create plan %q: %v", planID, err)
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
					planID, prevRate, pgtype.Int8{Int64: prevCap.Int64, Valid: true}, prevName,
				)
			} else {
				_, err = ts.DB.Exec(cctx,
					`UPDATE plans SET rate_limit_per_minute = $2, monthly_unit_cap = NULL, display_name = $3 WHERE id = $1`,
					planID, prevRate, prevName,
				)
			}
			if err != nil {
				t.Logf("harness: restore plan %q: %v", planID, err)
			}
		} else {
			if _, err := ts.DB.Exec(cctx, `DELETE FROM plans WHERE id = $1`, planID); err != nil {
				t.Logf("harness: cleanup plan %q: %v", planID, err)
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
	if len(email) > 254 {
		t.Fatalf("harness: CreateCustomer email exceeds RFC 5321 maximum length of 254 characters: %d", len(email))
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
	if err := ts.DB.QueryRow(planCtx, `SELECT 1 FROM plans WHERE id = $1`, planID).Scan(&dummy); errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("harness: CreateCustomer planID %q does not exist", planID)
	} else if err != nil {
		t.Fatalf("harness: CreateCustomer planID %q lookup failed: %v", planID, err)
	}

	// auth.Generate returns (full, prefix, error):
	//   apiKey = TestAPIKeyPrefix + "live_" + base32_suffix  (the complete API key callers present as Bearer)
	//   prefix = apiKey[:PrefixLen]                          (stored in DB for fast prefix lookup)
	apiKey, prefix, err := auth.Generate(TestAPIKeyPrefix)
	if err != nil {
		t.Fatalf("harness: generate api key: %v", err)
	}
	customerID := uuid.New()
	// cleanupPrefix names the prefix value used in the cleanup closure; strings
	// are value-copied by Go closures so no reference-capture risk exists, but the
	// named variable makes clear which value the Redis DEL key is derived from.
	cleanupPrefix := prefix

	// inserted is set to true only after both DB inserts succeed. The cleanup
	// closure checks it so it no-ops if CreateCustomer fatals before the rows exist.
	var inserted bool
	t.Cleanup(func() {
		if !inserted {
			return
		}
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
				"auth:"+cleanupPrefix,
			).Err(); delErr != nil && !errors.Is(delErr, context.DeadlineExceeded) && !errors.Is(delErr, context.Canceled) {
				t.Logf("harness: redis cleanup for customer %s: %v", cid, delErr)
			}
		}()
		cleanupErr := func(table string, opErr error) {
			logCleanupErr(t, customerID, table, opErr)
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
		var finalKeyErr error
		for attempt := 1; attempt <= maxCleanupRetries; attempt++ {
			if cctx.Err() != nil {
				finalKeyErr = cctx.Err()
				break
			}
			if attempt > 1 && !ctxSleep(cctx, cleanupRetryBackoff) {
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

	hash := auth.Hash(TestSalt(), apiKey)
	keyCtx, keyCancel := context.WithTimeout(context.Background(), customerInsertTimeout)
	defer keyCancel()
	_, err = ts.DB.Exec(keyCtx,
		`INSERT INTO api_keys (customer_id, prefix, hash) VALUES ($1, $2, $3)`,
		customerID, prefix, hash,
	)
	if err != nil {
		t.Fatalf("harness: insert api key for customer %s: %v", customerID, err)
	}
	inserted = true

	return customerID, apiKey
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
