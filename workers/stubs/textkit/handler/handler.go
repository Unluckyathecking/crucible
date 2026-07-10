// Package handler implements the textkit worker's operation dispatch:
// count-words, transform, and slugify. It is the reference implementation for
// a worker that branches on Request.Operation, in contrast to the single-op
// echo stub in workers/stubs/go.
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// Operation names forwarded opaquely by the gateway; these must match the
// openapi.RouteDescriptor.Operation values declared for this worker's routes
// (see gateway/test/textkit/routes.go).
const (
	OpCountWords = "count-words"
	OpTransform  = "transform"
	OpSlugify    = "slugify"
)

type countWordsRequest struct {
	Text string `json:"text"`
}

type countWordsResponse struct {
	Words int `json:"words"`
}

type transformRequest struct {
	Text string `json:"text"`
	Mode string `json:"mode"`
}

type transformResponse struct {
	Text string `json:"text"`
}

type slugifyRequest struct {
	Text string `json:"text"`
}

type slugifyResponse struct {
	Slug string `json:"slug"`
}

// Handle dispatches on in.Operation to one of textkit's operations.
func Handle(_ context.Context, in crucible.Request) (crucible.Response, error) {
	switch in.Operation {
	case OpCountWords:
		return countWords(in.Payload)
	case OpTransform:
		return transform(in.Payload)
	case OpSlugify:
		return slugify(in.Payload)
	default:
		return crucible.Response{}, &crucible.Error{
			Code:    "UNKNOWN_OPERATION",
			Message: fmt.Sprintf("textkit: unknown operation %q", in.Operation),
		}
	}
}

// countWords meters a computed quantity (word count) rather than a flat 1,
// proving variable billable_units through the frozen worker contract.
func countWords(payload json.RawMessage) (crucible.Response, error) {
	var req countWordsRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return crucible.Response{}, &crucible.Error{Code: "BAD_PAYLOAD", Message: err.Error()}
	}
	words := len(strings.Fields(req.Text))
	units := uint64(words)
	if units == 0 {
		// crucible.Serve forces BillableUnits to 1 when zero anyway; setting it
		// explicitly here keeps the returned payload's word count consistent
		// with what was billed.
		units = 1
	}
	return crucible.Response{
		Payload:       countWordsResponse{Words: words},
		BillableUnits: units,
		UnitsLabel:    "words",
	}, nil
}

func transform(payload json.RawMessage) (crucible.Response, error) {
	var req transformRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return crucible.Response{}, &crucible.Error{Code: "BAD_PAYLOAD", Message: err.Error()}
	}
	var out string
	switch req.Mode {
	case "upper":
		out = strings.ToUpper(req.Text)
	case "lower":
		out = strings.ToLower(req.Text)
	case "title":
		out = titleCase(req.Text)
	default:
		// Unreachable when the gateway's RequestSchema enum is enforced upstream;
		// kept as a defensive fallback for direct/out-of-band worker calls.
		return crucible.Response{}, &crucible.Error{Code: "BAD_PAYLOAD", Message: fmt.Sprintf("unsupported mode %q", req.Mode)}
	}
	return crucible.Response{
		Payload:       transformResponse{Text: out},
		BillableUnits: 1,
		UnitsLabel:    "operations",
	}, nil
}

func slugify(payload json.RawMessage) (crucible.Response, error) {
	var req slugifyRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return crucible.Response{}, &crucible.Error{Code: "BAD_PAYLOAD", Message: err.Error()}
	}
	return crucible.Response{
		Payload:       slugifyResponse{Slug: slug(req.Text)},
		BillableUnits: 1,
		UnitsLabel:    "slugs",
	}, nil
}

// titleCase upper-cases the first rune of each whitespace-separated word.
// strings.Title is deprecated and Unicode-boundary-unaware; this is
// sufficient for the ASCII-oriented reference worker.
func titleCase(s string) string {
	fields := strings.Fields(s)
	for i, f := range fields {
		r := []rune(f)
		r[0] = unicode.ToUpper(r[0])
		fields[i] = string(r)
	}
	return strings.Join(fields, " ")
}

// slug lowercases s and collapses runs of non-alphanumeric characters into a
// single hyphen, trimming any leading or trailing hyphen.
func slug(s string) string {
	var b strings.Builder
	atBoundary := true // suppresses a leading hyphen
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			atBoundary = false
			continue
		}
		if !atBoundary {
			b.WriteByte('-')
			atBoundary = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}
