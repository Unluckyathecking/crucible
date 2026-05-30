package auth

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestEnqueueUpdate_Bounded proves the last_used_at background work is bounded:
// during a cold-cache storm (e.g. Redis outage, every request is a cold path) the
// per-lookup enqueue is non-blocking and sheds excess work once the buffer is full,
// instead of spawning an unbounded goroutine per request. Pre-fix, Lookup spawned a
// detached `go func()` per cold lookup, so a burst of N lookups produced up to N
// concurrent DB writes with no upper bound — this test would not hold there.
func TestEnqueueUpdate_Bounded(t *testing.T) {
	const buf = 8
	// Construct the Store directly (white-box) so no real worker drains the buffer;
	// this lets us observe the cap deterministically without Postgres/Redis.
	s := &Store{updates: make(chan uuid.UUID, buf)}

	const burst = 1000
	queued := 0
	for i := 0; i < burst; i++ {
		if s.enqueueUpdate(uuid.New()) {
			queued++
		}
	}

	// At most `buf` updates are ever in flight; everything else is dropped. The number
	// of pending writes is capped by the buffer, NOT by the inbound request rate.
	if queued != buf {
		t.Fatalf("queued=%d under burst of %d, want exactly buffer size %d (excess must be dropped)", queued, burst, buf)
	}
	if len(s.updates) != buf {
		t.Fatalf("channel depth=%d, want %d — bound must equal buffer capacity", len(s.updates), buf)
	}
}

// TestEnqueueUpdate_NeverBlocks proves the hot path never blocks even when every
// caller in a concurrent burst tries to enqueue against a full, undrained buffer.
// A blocking (unbounded-backpressure) implementation would deadlock this test.
func TestEnqueueUpdate_NeverBlocks(t *testing.T) {
	s := &Store{updates: make(chan uuid.UUID, 4)}

	done := make(chan struct{})
	go func() {
		var wg sync.WaitGroup
		for i := 0; i < 200; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.enqueueUpdate(uuid.New()) // must return promptly whether queued or dropped
			}()
		}
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueUpdate blocked under concurrent burst against a full buffer; hot path must never block")
	}
}

// TestEnqueueUpdate_BoundIsIndependentOfBurstSize proves the genuine value of the
// non-blocking enqueue: the number of pending last_used_at writes is O(buffer), never
// O(requests). It drives bursts that are 2x, 10x, and 100x the buffer against an
// undrained queue and asserts the pending depth equals the buffer capacity every time —
// excess is shed, the queue never grows unbounded, and the hot path never blocks
// regardless of how large the cold-cache storm gets.
func TestEnqueueUpdate_BoundIsIndependentOfBurstSize(t *testing.T) {
	const buf = 16
	for _, burst := range []int{buf * 2, buf * 10, buf * 100} {
		s := &Store{updates: make(chan uuid.UUID, buf)}

		queued := 0
		for i := 0; i < burst; i++ {
			if s.enqueueUpdate(uuid.New()) {
				queued++
			}
		}

		if queued != buf {
			t.Fatalf("burst=%d: queued=%d, want exactly buffer size %d (excess must be dropped, not buffered)", burst, queued, buf)
		}
		if len(s.updates) != buf {
			t.Fatalf("burst=%d: pending depth=%d, want %d — bound must stay at buffer capacity, not grow with the burst", burst, len(s.updates), buf)
		}
	}
}

// TestClose_DrainsThenCancels proves Close has drain-then-cancel semantics: it finishes
// the writes already queued (not discarding recent telemetry) and then tears down rootCtx
// so no derived context can outlive the Store. The earlier version only asserted
// rootCtx.Err()!=nil after Close (trivially true once cancel runs anywhere); this also
// checks the queued work was actually drained, which is the property Close exists for.
func TestClose_DrainsThenCancels(t *testing.T) {
	const queuedWrites = 5
	var drained int64
	rootCtx, cancel := context.WithCancel(context.Background())
	s := &Store{
		updates: make(chan uuid.UUID, queuedWrites),
		rootCtx: rootCtx,
		cancel:  cancel,
	}
	// White-box drain worker mirroring processUpdates' loop shape, but counting instead of
	// hitting Postgres — it must consume every queued item before Close returns.
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for range s.updates {
			atomic.AddInt64(&drained, 1)
		}
	}()

	for i := 0; i < queuedWrites; i++ {
		if !s.enqueueUpdate(uuid.New()) {
			t.Fatalf("enqueue %d dropped while buffer had room", i)
		}
	}

	s.Close()

	if got := atomic.LoadInt64(&drained); got != queuedWrites {
		t.Fatalf("drained=%d, want %d — Close must drain queued writes, not abandon them", got, queuedWrites)
	}
	if s.rootCtx.Err() == nil {
		t.Fatal("rootCtx not cancelled after Close; derived writes could outlive the Store")
	}
}
