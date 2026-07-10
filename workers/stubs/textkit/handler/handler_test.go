package handler

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// TestCountWords exercises variable billable_units and the "words" units label across
// empty, single, multi-word, and whitespace-heavy inputs.
func TestCountWords(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		text        string
		wantWords   int
		wantUnits   uint64
		wantErrCode string
	}{
		{name: "empty", text: "", wantErrCode: "BAD_PAYLOAD"},
		{name: "whitespace-only", text: "   \t\n  ", wantErrCode: "BAD_PAYLOAD"},
		{name: "single-word", text: "hello", wantWords: 1, wantUnits: 1},
		{name: "two-words", text: "hello world", wantWords: 2, wantUnits: 2},
		{name: "whitespace-heavy", text: "  foo   bar   baz  ", wantWords: 3, wantUnits: 3},
		{name: "tab-separated", text: "a\tb\tc", wantWords: 3, wantUnits: 3},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := Handle(context.Background(), crucible.Request{
				Operation: OpCountWords,
				Payload:   mustJSON(countWordsRequest{Text: tc.text}),
			})
			if tc.wantErrCode != "" {
				var cerr *crucible.Error
				if !errors.As(err, &cerr) {
					t.Fatalf("want *crucible.Error with code %q, got err=%v", tc.wantErrCode, err)
				}
				if cerr.Code != tc.wantErrCode {
					t.Fatalf("want error code %q, got %q", tc.wantErrCode, cerr.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.BillableUnits != tc.wantUnits {
				t.Errorf("BillableUnits: want %d, got %d", tc.wantUnits, resp.BillableUnits)
			}
			if resp.UnitsLabel != "words" {
				t.Errorf("UnitsLabel: want %q, got %q", "words", resp.UnitsLabel)
			}
			got, ok := resp.Payload.(countWordsResponse)
			if !ok {
				t.Fatalf("Payload type: want countWordsResponse, got %T", resp.Payload)
			}
			if got.Words != tc.wantWords {
				t.Errorf("Words: want %d, got %d", tc.wantWords, got.Words)
			}
		})
	}
}

// TestTransform covers upper/lower/title modes and the BAD_PAYLOAD branch for an unknown mode.
func TestTransform(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		text        string
		mode        string
		wantText    string
		wantErrCode string
	}{
		{name: "upper", text: "hello world", mode: "upper", wantText: "HELLO WORLD"},
		{name: "lower", text: "HELLO WORLD", mode: "lower", wantText: "hello world"},
		{name: "title", text: "hello world", mode: "title", wantText: "Hello World"},
		{name: "title-mixed-case", text: "the QUICK brown FOX", mode: "title", wantText: "The QUICK Brown FOX"},
		{name: "unknown-mode", text: "hello", mode: "reverse", wantErrCode: "BAD_PAYLOAD"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := Handle(context.Background(), crucible.Request{
				Operation: OpTransform,
				Payload:   mustJSON(transformRequest{Text: tc.text, Mode: tc.mode}),
			})
			if tc.wantErrCode != "" {
				var cerr *crucible.Error
				if !errors.As(err, &cerr) {
					t.Fatalf("want *crucible.Error with code %q, got err=%v", tc.wantErrCode, err)
				}
				if cerr.Code != tc.wantErrCode {
					t.Fatalf("want error code %q, got %q", tc.wantErrCode, cerr.Code)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.BillableUnits != 1 {
				t.Errorf("BillableUnits: want 1, got %d", resp.BillableUnits)
			}
			got, ok := resp.Payload.(transformResponse)
			if !ok {
				t.Fatalf("Payload type: want transformResponse, got %T", resp.Payload)
			}
			if got.Text != tc.wantText {
				t.Errorf("Text: want %q, got %q", tc.wantText, got.Text)
			}
		})
	}
}

// TestSlugify exercises leading/trailing/collapsed non-alphanumeric runs via Handle.
func TestSlugify(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		text     string
		wantSlug string
	}{
		{name: "simple", text: "hello world", wantSlug: "hello-world"},
		{name: "leading-non-alnum", text: "--hello world", wantSlug: "hello-world"},
		{name: "trailing-non-alnum", text: "hello world--", wantSlug: "hello-world"},
		{name: "collapsed-run", text: "hello---world", wantSlug: "hello-world"},
		{name: "mixed-punct", text: "Hello, World!", wantSlug: "hello-world"},
		{name: "all-punct", text: "---", wantSlug: ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := Handle(context.Background(), crucible.Request{
				Operation: OpSlugify,
				Payload:   mustJSON(slugifyRequest{Text: tc.text}),
			})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.BillableUnits != 1 {
				t.Errorf("BillableUnits: want 1, got %d", resp.BillableUnits)
			}
			got, ok := resp.Payload.(slugifyResponse)
			if !ok {
				t.Fatalf("Payload type: want slugifyResponse, got %T", resp.Payload)
			}
			if got.Slug != tc.wantSlug {
				t.Errorf("Slug: want %q, got %q", tc.wantSlug, got.Slug)
			}
		})
	}
}

// TestHandleUnknownOperation verifies the default switch branch returns UNKNOWN_OPERATION.
func TestHandleUnknownOperation(t *testing.T) {
	t.Parallel()
	_, err := Handle(context.Background(), crucible.Request{
		Operation: "no-such-op",
		Payload:   mustJSON(map[string]any{}),
	})
	var cerr *crucible.Error
	if !errors.As(err, &cerr) {
		t.Fatalf("want *crucible.Error, got %v", err)
	}
	if cerr.Code != "UNKNOWN_OPERATION" {
		t.Fatalf("want code UNKNOWN_OPERATION, got %q", cerr.Code)
	}
}

// TestTitleCase exercises first-rune upper-casing on the unexported titleCase helper.
func TestTitleCase(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"hello world", "Hello World"},
		{"hello", "Hello"},
		{"already Title", "Already Title"},
		{"HELLO WORLD", "HELLO WORLD"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := titleCase(tc.in); got != tc.want {
				t.Errorf("titleCase(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSlug exercises leading/trailing trim and collapsed runs on the unexported slug helper.
func TestSlug(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
	}{
		{"hello world", "hello-world"},
		{"--hello  world--", "hello-world"},
		{"Hello World!", "hello-world"},
		{"foo--bar", "foo-bar"},
		{"abc", "abc"},
		{"  ", ""},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.in, func(t *testing.T) {
			t.Parallel()
			if got := slug(tc.in); got != tc.want {
				t.Errorf("slug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
