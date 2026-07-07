// Package paging is the single shared page/per-page query parsing, clamping,
// offset computation, and response envelope used by every /v1 and
// /v1/admin list endpoint. Before this package existed, operator and
// webhookout each independently duplicated an identical Page[T]{Items,Total}
// envelope and clamp block, and neither guarded against a pathological page
// number producing an unbounded (or, pre-clamp, overflowing) SQL OFFSET the
// way selferrors already did — this package fixes both by giving every
// clone-and-adapt product one place to inherit the plumbing from.
package paging

import (
	"errors"
	"net/url"
	"strconv"
)

// MaxOffset bounds page*perPage across every /v1 and /v1/admin list endpoint,
// so a pathological page number can never force an unbounded OFFSET scan.
// selferrors enforced this exact value before this package existed; it is
// now the framework-wide bound every list endpoint shares.
const MaxOffset = 10_000_000

// ErrPageTooLarge is returned by Offset when page/perPage would push the
// computed OFFSET past MaxOffset. Callers map this to 400 Bad Request.
var ErrPageTooLarge = errors.New("paging: page too large")

// Params is a raw, unclamped page/size pair straight off a query string. Zero
// means "absent" — feed straight into Clamp for the defaulting behavior every
// list endpoint shares.
type Params struct {
	Page    int
	PerPage int
}

// ParseQuery extracts "page" and sizeParam (e.g. "per_page", or selferrors'
// legacy "limit") from raw query values. A missing, non-numeric, or
// non-positive value comes back as 0.
func ParseQuery(q url.Values, sizeParam string) Params {
	return Params{
		Page:    parsePositiveInt(q.Get("page")),
		PerPage: parsePositiveInt(q.Get(sizeParam)),
	}
}

func parsePositiveInt(v string) int {
	if v == "" {
		return 0
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0
	}
	return n
}

// Clamp normalizes page/perPage the way every /v1 list endpoint already did
// independently before this package existed: perPage <= 0 defaults to
// defaultPerPage, perPage > maxPerPage is capped at maxPerPage (matching
// selferrors' documented "default 50, capped at 200" contract), and page < 1
// defaults to 1.
func Clamp(page, perPage, defaultPerPage, maxPerPage int) (int, int) {
	switch {
	case perPage <= 0:
		perPage = defaultPerPage
	case perPage > maxPerPage:
		perPage = maxPerPage
	}
	if page < 1 {
		page = 1
	}
	return page, perPage
}

// Offset computes the SQL OFFSET for an already-clamped, 1-based page and
// perPage, rejecting a page magnitude that would push page*perPage past
// MaxOffset — the guard that keeps a huge ?page= value from becoming an
// unbounded Postgres OFFSET scan or overflowing the multiplication.
func Offset(page, perPage int) (int, error) {
	if perPage > 0 && page-1 > MaxOffset/perPage {
		return 0, ErrPageTooLarge
	}
	return (page - 1) * perPage, nil
}

// Page is the standard paginated response envelope: the requested page's
// items plus the total matching row count across all pages.
type Page[T any] struct {
	Items []T   `json:"items"`
	Total int64 `json:"total"`
}

// Slice returns the [offset, offset+perPage) window of items, clamped to
// items' bounds. For list endpoints that paginate over an
// already-materialized in-process slice rather than pushing LIMIT/OFFSET
// into SQL (e.g. because the underlying store method's signature is out of
// this change's scope).
func Slice[T any](items []T, offset, perPage int) []T {
	if offset < 0 || offset >= len(items) {
		return []T{}
	}
	end := len(items)
	if perPage > 0 && offset+perPage < end {
		end = offset + perPage
	}
	return items[offset:end]
}
