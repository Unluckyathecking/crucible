// Package textkit declares the route table and end-to-end harness test for
// the textkit reference worker (workers/stubs/textkit). It proves the
// gateway's per-route SampleRequest/RequestSchema primitives against a
// worker that dispatches on Request.Operation across multiple operations,
// something no other worker in the tree does (every other stub is a
// single-operation echo).
package textkit

import (
	"encoding/json"

	"github.com/Unluckyathecking/crucible/gateway/internal/openapi"
	"github.com/Unluckyathecking/crucible/workers/stubs/textkit/handler"
)

var minLenOne = 1

// noAdditional rejects any request-body field not named in a route's
// Properties, matching the RequestSchema for every textkit operation.
func noAdditional() *openapi.Schema { return &openapi.Schema{BoolFalse: true} }

// Routes is the textkit product's /v1 route table, driven end-to-end by
// TestTextkitOperations via harness.Options.Routes. It is intentionally not
// registered in gateway/internal/server/routes_table.go — the harness swaps
// it in for the duration of a test, leaving the default template's V1Routes
// (/echo only) untouched.
var Routes = []openapi.RouteDescriptor{
	{
		Path:          "/textkit/count-words",
		Operation:     handler.OpCountWords,
		Summary:       "Count words in input text",
		SampleRequest: json.RawMessage(`{"text":"the quick brown fox jumps over the lazy dog"}`),
		RequestSchema: &openapi.Schema{
			Type:     "object",
			Required: []string{"text"},
			Properties: map[string]*openapi.Schema{
				// Pattern rejects whitespace-only text: strings.Fields would count
				// zero words for it, which would otherwise bill 1 unit for a
				// response reporting words:0.
				"text": {Type: "string", MinLength: &minLenOne, Pattern: `\S`},
			},
			AdditionalProperties: noAdditional(),
		},
	},
	{
		Path:          "/textkit/transform",
		Operation:     handler.OpTransform,
		Summary:       "Change the case of input text (upper, lower, or title)",
		SampleRequest: json.RawMessage(`{"text":"Hello World","mode":"upper"}`),
		RequestSchema: &openapi.Schema{
			Type:     "object",
			Required: []string{"text", "mode"},
			Properties: map[string]*openapi.Schema{
				"text": {Type: "string", MinLength: &minLenOne},
				"mode": {Type: "string", Enum: []any{"upper", "lower", "title"}},
			},
			AdditionalProperties: noAdditional(),
		},
	},
	{
		Path:          "/textkit/slugify",
		Operation:     handler.OpSlugify,
		Summary:       "Slugify input text into a URL-safe slug",
		SampleRequest: json.RawMessage(`{"text":"Hello, World! 2026"}`),
		RequestSchema: &openapi.Schema{
			Type:     "object",
			Required: []string{"text"},
			Properties: map[string]*openapi.Schema{
				"text": {Type: "string", MinLength: &minLenOne},
			},
			AdditionalProperties: noAdditional(),
		},
	},
}
