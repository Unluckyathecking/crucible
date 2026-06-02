package billing

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pashagolub/pgxmock/v5"
)

// int64ptr returns a pointer to the given int64 value, for use in mock rows
// that must satisfy the nullable *int64 column in the plans table.
func int64ptr(v int64) *int64 { return &v }

// waitReloadApplied blocks until a background (async) reload has populated planID
// with the expected rate and is no longer loading, or fails the test after a short
// deadline. It reads cache state directly under the lock so it never itself triggers
// a reload. Used by the stale-path tests, whose reload runs in a goroutine.
func waitReloadApplied(t *testing.T, pc *PlanCache, planID string, wantRate int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		pc.mu.RLock()
		e, ok := pc.plans[planID]
		loading := pc.loading
		pc.mu.RUnlock()
		if ok && e.RatePerMinute == wantRate && !loading {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background reload did not apply %s=%d (loading=%v)", planID, wantRate, loading)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestPlanCache_Concurrency_Race exercises concurrent Get calls under -race.
// A stale cache is set up so that the first goroutine triggers reload while
// others read stale values — the single-flight guard must prevent DB fan-out.
func TestPlanCache_Concurrency_Race(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	// Only ONE reload query is expected — the single-flight guard blocks the rest.
	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnRows(mock.NewRows([]string{"id", "rate_limit_per_minute", "monthly_unit_cap"}).
			AddRow("pro", 200, int64ptr(5000)).
			AddRow("free", 60, (*int64)(nil)))

	// Start with a stale cache so reload fires.
	pc := &PlanCache{
		db:      mock,
		baseCtx: context.Background(),
		plans:   map[string]PlanEntry{"pro": {RatePerMinute: 100, MonthlyCap: 9999}},
		fresh:   time.Now().Add(-2 * cacheTTL),
	}

	const goroutines = 20
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			_ = pc.Get(context.Background(), "pro")
		}()
	}
	wg.Wait()

	// The stale reads return last-known immediately and fire a single background
	// reload; wait for it to land before asserting on the refreshed value.
	waitReloadApplied(t, pc, "pro", 200)

	// Enforce the single-query expectation: if more than one reload fired,
	// pgxmock will report an unexpected call here.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("single-flight violated — mock expectations not met: %v", err)
	}

	// After reload the new value should be visible.
	entry := pc.Get(context.Background(), "pro")
	if entry.RatePerMinute != 200 {
		t.Errorf("RatePerMinute = %d, want 200 after reload", entry.RatePerMinute)
	}
	if entry.MonthlyCap != 5000 {
		t.Errorf("MonthlyCap = %d, want 5000 after reload", entry.MonthlyCap)
	}
}

// TestPlanCache_TTL_RefreshTriggered verifies that a stale cache (age > cacheTTL)
// causes a DB reload and updates the in-memory plans map.
func TestPlanCache_TTL_RefreshTriggered(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnRows(mock.NewRows([]string{"id", "rate_limit_per_minute", "monthly_unit_cap"}).
			AddRow("enterprise", 1000, int64ptr(100000)))

	pc := &PlanCache{
		db:      mock,
		baseCtx: context.Background(),
		plans:   map[string]PlanEntry{},
		fresh:   time.Now().Add(-2 * cacheTTL), // stale → background (async) reload
	}

	// A stale read returns immediately and kicks off a background reload; wait for
	// it to land before asserting the refreshed values.
	_ = pc.Get(context.Background(), "enterprise")
	waitReloadApplied(t, pc, "enterprise", 1000)

	entry := pc.Get(context.Background(), "enterprise")
	if entry.RatePerMinute != 1000 {
		t.Errorf("RatePerMinute = %d, want 1000 after TTL-triggered reload", entry.RatePerMinute)
	}
	if entry.MonthlyCap != 100000 {
		t.Errorf("MonthlyCap = %d, want 100000 after TTL-triggered reload", entry.MonthlyCap)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestPlanCache_TTL_FreshSkipsReload verifies that a fresh cache does NOT hit the DB.
func TestPlanCache_TTL_FreshSkipsReload(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	// No expectations — any DB call would fail.
	pc := &PlanCache{
		db: mock,
		plans: map[string]PlanEntry{
			"basic": {RatePerMinute: 30, MonthlyCap: 500},
		},
		fresh: time.Now(), // not stale
	}

	entry := pc.Get(context.Background(), "basic")
	if entry.RatePerMinute != 30 {
		t.Errorf("RatePerMinute = %d, want 30 (fresh cache, no reload)", entry.RatePerMinute)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unexpected DB calls: %v", err)
	}
}

// TestPlanCache_Reload_DBError verifies that a DB error during reload keeps
// the previous in-memory values intact (fail-open on last-known values).
func TestPlanCache_Reload_DBError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnError(fmt.Errorf("connection refused"))

	// Zero (cold) fresh → reload runs synchronously, so the single Get below
	// deterministically observes the fail-open outcome. The stale path's async
	// reload is covered in plans_async_test.go.
	pc := &PlanCache{
		db:    mock,
		plans: map[string]PlanEntry{"pro": {RatePerMinute: 77, MonthlyCap: 777}},
	}

	// Should return last-known value despite DB error.
	entry := pc.Get(context.Background(), "pro")
	if entry.RatePerMinute != 77 {
		t.Errorf("RatePerMinute = %d, want 77 (last-known after DB error)", entry.RatePerMinute)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestPlanCache_Reload_IterationError verifies that a row iteration error keeps
// the previous in-memory values intact.
func TestPlanCache_Reload_IterationError(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	// CloseError sets the error returned by rows.Err() after iteration.
	rows := mock.NewRows([]string{"id", "rate_limit_per_minute", "monthly_unit_cap"}).
		CloseError(fmt.Errorf("network error mid-iteration"))

	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnRows(rows)

	// Zero (cold) fresh → synchronous reload for a deterministic single-Get assertion.
	pc := &PlanCache{
		db:    mock,
		plans: map[string]PlanEntry{"pro": {RatePerMinute: 42, MonthlyCap: 42}},
	}

	// Even with iteration error, the code should not panic and last-known is preserved.
	entry := pc.Get(context.Background(), "pro")
	// Reload failed mid-iteration so last-known values are kept.
	if entry.RatePerMinute != 42 {
		t.Errorf("RatePerMinute = %d, want 42 (last-known after iteration error)", entry.RatePerMinute)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestPlanCache_Reload_NullCap verifies that a NULL monthly_unit_cap is stored as 0
// (unlimited) in the PlanEntry.
func TestPlanCache_Reload_NullCap(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	// NULL monthly_unit_cap must come through as 0 (unlimited).
	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnRows(mock.NewRows([]string{"id", "rate_limit_per_minute", "monthly_unit_cap"}).
			AddRow("unlimited", 999, (*int64)(nil)))

	// Zero (cold) fresh → synchronous reload for a deterministic single-Get assertion.
	pc := &PlanCache{
		db:    mock,
		plans: map[string]PlanEntry{},
	}

	entry := pc.Get(context.Background(), "unlimited")
	if entry.RatePerMinute != 999 {
		t.Errorf("RatePerMinute = %d, want 999", entry.RatePerMinute)
	}
	if entry.MonthlyCap != 0 {
		t.Errorf("MonthlyCap = %d, want 0 (unlimited from NULL)", entry.MonthlyCap)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}

// TestPlanCache_Concurrency_MultipleReloads verifies that two consecutive
// stale windows each trigger exactly one reload.
func TestPlanCache_Concurrency_MultipleReloads(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock: %v", err)
	}
	defer mock.Close()

	// First reload.
	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnRows(mock.NewRows([]string{"id", "rate_limit_per_minute", "monthly_unit_cap"}).
			AddRow("pro", 100, int64ptr(1000)))

	pc := &PlanCache{
		db:      mock,
		baseCtx: context.Background(),
		plans:   map[string]PlanEntry{},
		fresh:   time.Now().Add(-2 * cacheTTL),
	}

	// First stale window triggers the first (background) reload.
	_ = pc.Get(context.Background(), "pro")
	waitReloadApplied(t, pc, "pro", 100)

	// Now set stale again and set up second reload expectation.
	pc.mu.Lock()
	pc.fresh = time.Now().Add(-2 * cacheTTL)
	pc.mu.Unlock()

	mock.ExpectQuery(`SELECT id, rate_limit_per_minute, monthly_unit_cap FROM plans`).
		WillReturnRows(mock.NewRows([]string{"id", "rate_limit_per_minute", "monthly_unit_cap"}).
			AddRow("pro", 200, int64ptr(2000)))

	// Second stale window triggers the second (background) reload.
	_ = pc.Get(context.Background(), "pro")
	waitReloadApplied(t, pc, "pro", 200)

	entry := pc.Get(context.Background(), "pro")
	if entry.RatePerMinute != 200 {
		t.Errorf("RatePerMinute = %d, want 200 after second reload", entry.RatePerMinute)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("mock expectations not met: %v", err)
	}
}
