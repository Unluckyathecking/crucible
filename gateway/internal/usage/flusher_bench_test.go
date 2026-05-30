package usage

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

func BenchmarkClaimAndEmitNewBatches(b *testing.B) {
	pool := newTestPool(b)
	ctx := context.Background()

	// Insert 100 customers and api keys
	var customers []uuid.UUID
	for i := 0; i < 100; i++ {
		custID := uuid.New()
		apiKeyID := uuid.New()
		email := fmt.Sprintf("bench_%s@test.local", custID)
		prefix := fmt.Sprintf("tst_%s", custID.String()[:8])

		_, err := pool.Exec(ctx,
			`INSERT INTO customers (id, email, plan_id, stripe_customer_id) VALUES ($1, $2, 'free', $3)`,
			custID, email, fmt.Sprintf("cus_%s", custID))
		if err != nil {
			b.Fatalf("insert customer: %v", err)
		}
		_, err = pool.Exec(ctx,
			`INSERT INTO api_keys (id, customer_id, prefix, hash, name) VALUES ($1, $2, $3, E'\\\\xdeadbeef', 'test-key')`,
			apiKeyID, custID, prefix)
		if err != nil {
			b.Fatalf("insert api_key: %v", err)
		}
		customers = append(customers, custID)
	}

	mock := &mockStripeMeter{}
	f := NewFlusher(pool, mock, 0)

	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Insert unbatched usage events for the 100 customers
		for _, custID := range customers {
			_, err := pool.Exec(ctx,
				`INSERT INTO usage_events (customer_id, api_key_id, operation, billable_units, request_id)
				VALUES ($1, (SELECT id FROM api_keys WHERE customer_id=$1 LIMIT 1), 'bench.op', 1, $2)`,
				custID, uuid.New().String())
			if err != nil {
				b.Fatalf("insert usage: %v", err)
			}
		}
		b.StartTimer()

		if err := f.claimAndEmitNewBatches(ctx); err != nil {
			b.Fatalf("claimAndEmitNewBatches: %v", err)
		}
	}
}
