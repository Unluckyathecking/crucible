package paging

import (
	"net/url"
	"testing"
	"time"
)

func TestParseDateRange(t *testing.T) {
	now := time.Date(2026, 3, 15, 13, 45, 0, 0, time.UTC)
	tomorrow := time.Date(2026, 3, 16, 0, 0, 0, 0, time.UTC)
	day := func(s string) time.Time {
		d, err := time.Parse("2006-01-02", s)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	tests := []struct {
		name     string
		query    string
		wantFrom time.Time
		wantTo   time.Time
		wantMsg  string
	}{
		{"no params uses defaults", "", tomorrow.AddDate(0, 0, -30), tomorrow, ""},
		{"valid range", "from=2026-03-01&to=2026-03-10", day("2026-03-01"), day("2026-03-11"), ""},
		{"to today is allowed", "to=2026-03-15", tomorrow.AddDate(0, 0, -30), tomorrow, ""},
		{"exactly max range", "from=2025-12-15&to=2026-03-15", day("2025-12-15"), tomorrow, ""},
		{"bad from format", "from=03-01-2026", time.Time{}, time.Time{}, "invalid 'from' date, expected ISO 8601"},
		{"from calendar overflow", "from=2026-02-30", time.Time{}, time.Time{}, "invalid 'from' date, expected ISO 8601"},
		{"bad to format", "to=yesterday", time.Time{}, time.Time{}, "invalid 'to' date, expected ISO 8601"},
		{"bad from wins over bad to", "from=x&to=y", time.Time{}, time.Time{}, "invalid 'from' date, expected ISO 8601"},
		{"from after to", "from=2026-03-10&to=2026-03-01", time.Time{}, time.Time{}, "'from' must not be after 'to'"},
		{"to in future", "to=2026-03-16", time.Time{}, time.Time{}, "'to' date must not be in the future"},
		{"from in future", "from=2026-03-17", time.Time{}, time.Time{}, "'from' date must not be in the future"},
		{"over max range", "from=2025-12-14&to=2026-03-15", time.Time{}, time.Time{}, "date range exceeds maximum of 90 days"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			q, err := url.ParseQuery(tc.query)
			if err != nil {
				t.Fatal(err)
			}
			from, to, msg := ParseDateRange(q, now)
			if msg != tc.wantMsg {
				t.Fatalf("msg = %q, want %q", msg, tc.wantMsg)
			}
			if !from.Equal(tc.wantFrom) || !to.Equal(tc.wantTo) {
				t.Fatalf("window = [%v, %v), want [%v, %v)", from, to, tc.wantFrom, tc.wantTo)
			}
		})
	}
}
