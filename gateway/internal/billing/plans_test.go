package billing

import (
	"context"
	"testing"
	"time"
)

func TestPlanCache_Get_CacheHit(t *testing.T) {
	now := time.Now()
	pc := &PlanCache{
		plans: map[string]PlanEntry{
			"pro":    {RatePerMinute: 120, MonthlyCap: 10000},
			"unlim":  {RatePerMinute: 300, MonthlyCap: 0},
		},
		fresh: now,
	}

	tests := []struct {
		name     string
		planID   string
		wantRate int
		wantCap  int64
	}{
		{"pro plan cached", "pro", 120, 10000},
		{"unlimited plan cached", "unlim", 300, 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entry := pc.Get(context.Background(), tc.planID)
			if entry.RatePerMinute != tc.wantRate {
				t.Errorf("RatePerMinute = %d, want %d", entry.RatePerMinute, tc.wantRate)
			}
			if entry.MonthlyCap != tc.wantCap {
				t.Errorf("MonthlyCap = %d, want %d", entry.MonthlyCap, tc.wantCap)
			}
		})
	}
}

func TestPlanCache_Get_MissFallback(t *testing.T) {
	pc := &PlanCache{
		plans: map[string]PlanEntry{},
		fresh: time.Now(),
	}

	entry := pc.Get(context.Background(), "super-enterprise-plus")
	if entry.RatePerMinute != 60 {
		t.Errorf("RatePerMinute = %d, want 60 (free-tier fallback)", entry.RatePerMinute)
	}
	if entry.MonthlyCap != 1000 {
		t.Errorf("MonthlyCap = %d, want 1000 (free-tier fallback)", entry.MonthlyCap)
	}
}

func TestPlanCache_reload_SingleFlight(t *testing.T) {
	pc := &PlanCache{db: nil, loading: true}
	pc.reload(context.Background())

	if pc.loading == false {
		t.Error("loading flag should remain true after early-exit in reload")
	}
}

func TestPlanCache_RatePerMinute(t *testing.T) {
	pc := &PlanCache{
		plans: map[string]PlanEntry{"pro": {RatePerMinute: 250, MonthlyCap: 0}},
		fresh: time.Now(),
	}

	if got := pc.RatePerMinute(context.Background(), "pro"); got != 250 {
		t.Errorf("RatePerMinute = %d, want 250", got)
	}
	if got := pc.RatePerMinute(context.Background(), "free"); got != 60 {
		t.Errorf("RatePerMinute for unknown = %d, want 60", got)
	}
}

func TestPlanCache_MonthlyCap(t *testing.T) {
	pc := &PlanCache{
		plans: map[string]PlanEntry{"pro": {RatePerMinute: 100, MonthlyCap: 5000}},
		fresh: time.Now(),
	}

	if got := pc.MonthlyCap(context.Background(), "pro"); got != 5000 {
		t.Errorf("MonthlyCap = %d, want 5000", got)
	}
	if got := pc.MonthlyCap(context.Background(), "unknown"); got != 1000 {
		t.Errorf("MonthlyCap for unknown = %d, want 1000", got)
	}
}

func TestPlanCache_NewPlanCache(t *testing.T) {
	pc := NewPlanCache(nil)
	if len(pc.plans) != 0 {
		t.Errorf("expected empty plans map, got %d entries", len(pc.plans))
	}
}
