package billing

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// fakePlanRow is one row the fake DB hands back to PlanCache.reload.
type fakePlanRow struct {
	id   string
	rate int
	cap  *int64
}

// fakePlanRows is a minimal pgx.Rows over a fixed slice of plan rows.
// Only the methods PlanCache.reload uses (Next/Scan/Err/Close) carry behaviour.
type fakePlanRows struct {
	rows []fakePlanRow
	pos  int
}

func (r *fakePlanRows) Next() bool {
	if r.pos >= len(r.rows) {
		return false
	}
	r.pos++
	return true
}

func (r *fakePlanRows) Scan(dest ...any) error {
	cur := r.rows[r.pos-1]
	*(dest[0].(*string)) = cur.id
	*(dest[1].(*int)) = cur.rate
	*(dest[2].(**int64)) = cur.cap
	return nil
}

func (r *fakePlanRows) Close()                                       {}
func (r *fakePlanRows) Err() error                                   { return nil }
func (r *fakePlanRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakePlanRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *fakePlanRows) Values() ([]any, error)                       { return nil, nil }
func (r *fakePlanRows) RawValues() [][]byte                          { return nil }
func (r *fakePlanRows) Conn() *pgx.Conn                              { return nil }

// fakeDB implements the package `db` interface, counting Query calls and
// optionally blocking each one so tests can assert single-flight and
// non-blocking-serve behaviour under concurrency.
type fakeDB struct {
	queries int64         // atomic: total Query calls
	block   chan struct{} // if non-nil, each Query blocks until it is closed
	rows    []fakePlanRow
}

func (d *fakeDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	atomic.AddInt64(&d.queries, 1)
	if d.block != nil {
		select {
		case <-d.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &fakePlanRows{rows: d.rows}, nil
}

func (d *fakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row { return nil }
func (d *fakeDB) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, nil
}

func cap64(v int64) *int64 { return &v }

// TestPlanCache_AsyncReload_SingleFlight: a stampede of concurrent stale reads
// must (a) be served immediately from the last-known-good value without blocking
// on the DB, and (b) trigger exactly one in-flight reload.
func TestPlanCache_AsyncReload_SingleFlight(t *testing.T) {
	block := make(chan struct{})
	fdb := &fakeDB{
		block: block,
		rows:  []fakePlanRow{{id: "pro", rate: 999, cap: cap64(42)}},
	}
	pc := &PlanCache{
		db:      fdb,
		baseCtx: context.Background(),
		plans:   map[string]PlanEntry{"pro": {RatePerMinute: 120, MonthlyCap: 10000}},
		fresh:   time.Now().Add(-2 * cacheTTL), // stale
	}

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	served := make(chan PlanEntry, n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			served <- pc.Get(context.Background(), "pro")
		}()
	}

	// All Get calls must return promptly with the stale-but-valid value even
	// though the (background) reload is still blocked on the DB.
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		close(block)
		t.Fatal("Get calls blocked on the in-flight DB reload (async reload regression)")
	}

	for i := 0; i < n; i++ {
		e := <-served
		if e.RatePerMinute != 120 || e.MonthlyCap != 10000 {
			t.Fatalf("served value = %+v, want stale-but-valid {120 10000}", e)
		}
	}

	// Exactly one reload should be in flight while we hold the block open.
	if got := atomic.LoadInt64(&fdb.queries); got != 1 {
		close(block)
		t.Fatalf("in-flight reload count = %d, want exactly 1 (single-flight)", got)
	}

	// Release the reload and let it apply fresh values.
	close(block)
	deadline := time.Now().Add(2 * time.Second)
	for {
		pc.mu.RLock()
		e, ok := pc.plans["pro"]
		loading := pc.loading
		pc.mu.RUnlock()
		if ok && e.RatePerMinute == 999 && !loading {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("background reload did not apply fresh values; got %+v loading=%v", e, loading)
		}
		time.Sleep(5 * time.Millisecond)
	}

	if got := atomic.LoadInt64(&fdb.queries); got != 1 {
		t.Fatalf("total reload count = %d, want 1", got)
	}
}

// TestPlanCache_ColdStart_BlocksAndPopulates: with no last-known value, the very
// first Get must block, run the reload synchronously, and return the real value
// (never an empty/free-tier-fallback mis-tier).
func TestPlanCache_ColdStart_BlocksAndPopulates(t *testing.T) {
	fdb := &fakeDB{
		rows: []fakePlanRow{
			{id: "pro", rate: 250, cap: cap64(5000)},
			{id: "unlim", rate: 300, cap: nil},
		},
	}
	pc := NewPlanCache(nil)
	pc.db = fdb // NewPlanCache wires baseCtx; swap in the fake DB.

	e := pc.Get(context.Background(), "pro")
	if e.RatePerMinute != 250 || e.MonthlyCap != 5000 {
		t.Fatalf("cold-start Get = %+v, want {250 5000} populated synchronously", e)
	}
	if got := atomic.LoadInt64(&fdb.queries); got != 1 {
		t.Fatalf("cold-start query count = %d, want 1", got)
	}

	// nullable cap maps to 0 (unlimited).
	if e := pc.Get(context.Background(), "unlim"); e.RatePerMinute != 300 || e.MonthlyCap != 0 {
		t.Fatalf("unlim = %+v, want {300 0}", e)
	}
	// Second read is a warm cache hit: no extra query.
	if got := atomic.LoadInt64(&fdb.queries); got != 1 {
		t.Fatalf("warm-read query count = %d, want 1 (no extra reload)", got)
	}
}

// TestPlanCache_AsyncReload_NoRace exercises concurrent Get + background reloads
// under -race to prove the mutex guards all shared state.
func TestPlanCache_AsyncReload_NoRace(t *testing.T) {
	fdb := &fakeDB{rows: []fakePlanRow{{id: "pro", rate: 100, cap: cap64(1)}}}
	pc := &PlanCache{
		db:      fdb,
		baseCtx: context.Background(),
		plans:   map[string]PlanEntry{"pro": {RatePerMinute: 1, MonthlyCap: 1}},
		fresh:   time.Now().Add(-2 * cacheTTL),
	}

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				_ = pc.Get(context.Background(), "pro")
			}
		}()
	}
	wg.Wait()
}
