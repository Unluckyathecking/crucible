package paging

import (
	"fmt"
	"net/url"
	"regexp"
	"time"
)

const (
	// DefaultRangeDays is the span of the default date window.
	DefaultRangeDays = 30
	// MaxRangeDays is the largest accepted span between from and to.
	MaxRangeDays = 90
)

var isoDateRE = regexp.MustCompile(`^\d{4}-(0[1-9]|1[0-2])-(0[1-9]|[12]\d|3[01])$`)

// ParseDateRange reads the optional "from" and "to" ISO dates (YYYY-MM-DD,
// both inclusive) from q and returns the window [from, toExclusive) in UTC.
// Defaults are [tomorrowMidnight - DefaultRangeDays, tomorrowMidnight), where
// tomorrowMidnight is derived from now. On invalid input it returns a
// non-empty errMsg for the caller to send as a 400 and zero times.
func ParseDateRange(q url.Values, now time.Time) (from, toExclusive time.Time, errMsg string) {
	now = now.UTC()
	tomorrowMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
	from = tomorrowMidnight.AddDate(0, 0, -DefaultRangeDays)
	toExclusive = tomorrowMidnight

	if v := q.Get("from"); v != "" {
		parsed, ok := parseISODate(v)
		if !ok {
			return time.Time{}, time.Time{}, "invalid 'from' date, expected ISO 8601"
		}
		from = parsed
	}
	if v := q.Get("to"); v != "" {
		parsed, ok := parseISODate(v)
		if !ok {
			return time.Time{}, time.Time{}, "invalid 'to' date, expected ISO 8601"
		}
		// `to` is inclusive; advance to the next midnight for the exclusive DB bound.
		toExclusive = parsed.AddDate(0, 0, 1)
	}
	if toExclusive.After(tomorrowMidnight) {
		return time.Time{}, time.Time{}, "'to' date must not be in the future"
	}
	if from.After(tomorrowMidnight) {
		return time.Time{}, time.Time{}, "'from' date must not be in the future"
	}
	userVisibleTo := toExclusive.AddDate(0, 0, -1)
	if from.After(userVisibleTo) {
		return time.Time{}, time.Time{}, "'from' must not be after 'to'"
	}
	rangeDays := int(userVisibleTo.Sub(from).Hours() / 24)
	if rangeDays < 0 || rangeDays > MaxRangeDays {
		return time.Time{}, time.Time{}, fmt.Sprintf("date range exceeds maximum of %d days", MaxRangeDays)
	}
	return from, toExclusive, ""
}

// parseISODate parses an ISO 8601 date (YYYY-MM-DD) as UTC midnight. Rejects
// calendar overflow (e.g. "2023-02-30") that time.Parse would otherwise
// silently normalize (Feb 30 -> Mar 2) via a round-trip format check.
func parseISODate(s string) (time.Time, bool) {
	if !isoDateRE.MatchString(s) {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	if t.Format("2006-01-02") != s {
		return time.Time{}, false
	}
	return t, true
}
