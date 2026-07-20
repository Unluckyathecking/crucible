package paging_test

import (
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	"github.com/Unluckyathecking/crucible/gateway/internal/paging"
)

func TestParseQuery_AbsentDefaultsToZero(t *testing.T) {
	q := url.Values{}
	p := paging.ParseQuery(q, "per_page")
	if p.Page != 0 || p.PerPage != 0 {
		t.Fatalf("ParseQuery(empty) = %+v, want zero value", p)
	}
}

func TestParseQuery_ParsesPageAndSizeParam(t *testing.T) {
	q := url.Values{"page": {"3"}, "limit": {"75"}}
	p := paging.ParseQuery(q, "limit")
	if p.Page != 3 || p.PerPage != 75 {
		t.Fatalf("ParseQuery = %+v, want {3 75}", p)
	}
}

func TestParseQuery_IgnoresNonPositiveAndMalformed(t *testing.T) {
	cases := []string{"0", "-5", "not-a-number", ""}
	for _, v := range cases {
		q := url.Values{"page": {v}, "per_page": {v}}
		p := paging.ParseQuery(q, "per_page")
		if p.Page != 0 || p.PerPage != 0 {
			t.Errorf("ParseQuery(%q) = %+v, want zero value", v, p)
		}
	}
}

func TestClamp_ZeroOrNegativePerPageDefaults(t *testing.T) {
	for _, in := range []int{0, -1, -100} {
		_, perPage := paging.Clamp(1, in, 20, 100)
		if perPage != 20 {
			t.Errorf("Clamp(1, %d, 20, 100) perPage = %d, want 20 (default)", in, perPage)
		}
	}
}

func TestClamp_OversizedPerPageCapsAtMax(t *testing.T) {
	_, perPage := paging.Clamp(1, 99999, 50, 200)
	if perPage != 200 {
		t.Errorf("Clamp(1, 99999, 50, 200) perPage = %d, want 200 (capped, not reset to default)", perPage)
	}
}

func TestClamp_PageBelowOneDefaultsToOne(t *testing.T) {
	for _, in := range []int{0, -1, -999} {
		page, _ := paging.Clamp(in, 20, 20, 100)
		if page != 1 {
			t.Errorf("Clamp(%d, ...) page = %d, want 1", in, page)
		}
	}
}

func TestClamp_ValidValuesPassThrough(t *testing.T) {
	page, perPage := paging.Clamp(5, 30, 20, 100)
	if page != 5 || perPage != 30 {
		t.Errorf("Clamp(5, 30, 20, 100) = (%d, %d), want (5, 30)", page, perPage)
	}
}

func TestOffset_ComputesZeroBasedOffset(t *testing.T) {
	cases := []struct{ page, perPage, want int }{
		{1, 20, 0},
		{2, 20, 20},
		{5, 10, 40},
	}
	for _, c := range cases {
		got, err := paging.Offset(c.page, c.perPage)
		if err != nil {
			t.Fatalf("Offset(%d, %d): unexpected error: %v", c.page, c.perPage, err)
		}
		if got != c.want {
			t.Errorf("Offset(%d, %d) = %d, want %d", c.page, c.perPage, got, c.want)
		}
	}
}

func TestOffset_RejectsPageTooLarge(t *testing.T) {
	// perPage=100: MaxOffset/perPage = 100_000, so page-1 > 100_000 must fail.
	_, err := paging.Offset(999_999_999, 100)
	if !errors.Is(err, paging.ErrPageTooLarge) {
		t.Fatalf("Offset(999999999, 100) err = %v, want ErrPageTooLarge", err)
	}
}

func TestOffset_BoundaryJustUnderMaxSucceeds(t *testing.T) {
	// perPage=100: largest page such that (page-1)*100 <= MaxOffset (10_000_000).
	page := paging.MaxOffset/100 + 1
	offset, err := paging.Offset(page, 100)
	if err != nil {
		t.Fatalf("Offset(%d, 100): unexpected error: %v", page, err)
	}
	if offset != paging.MaxOffset {
		t.Errorf("Offset(%d, 100) = %d, want %d", page, offset, paging.MaxOffset)
	}

	if _, err := paging.Offset(page+1, 100); !errors.Is(err, paging.ErrPageTooLarge) {
		t.Errorf("Offset(%d, 100) err = %v, want ErrPageTooLarge", page+1, err)
	}
}

func TestPage_MarshalsItemsAndTotal(t *testing.T) {
	p := paging.Page[int]{Items: []int{1, 2, 3}, Total: 42}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if _, ok := out["items"]; !ok {
		t.Error(`marshaled Page is missing "items" key`)
	}
	if total, ok := out["total"].(float64); !ok || total != 42 {
		t.Errorf(`marshaled Page "total" = %v, want 42`, out["total"])
	}
}
