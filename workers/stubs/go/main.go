// Hello-world Crucible worker stub.
//
// Every Go worker in a Crucible clone starts from this shape: import the SDK,
// implement one function, call Serve. Per-product logic lives entirely in the
// handler body.
//
// Run locally:  go run .   (or `make worker` from the repo root)
//
// Smoke test:
//
//	curl -X POST localhost:8081/invoke \
//	  -H 'content-type: application/json' \
//	  -d '{"operation":"echo","payload":{"x":"hi"},"metadata":{"units":"3"}}'
package main

import (
	"context"
	"os"
	"strconv"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

func main() {
	port := 8081
	if v := os.Getenv("PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			port = n
		}
	}
	if err := crucible.Serve(port, handle); err != nil {
		panic(err)
	}
}

// handle echoes the request payload back. If metadata["units"] is set to a positive integer,
// it returns that as billable_units — useful for testing per-unit billing end-to-end.
func handle(_ context.Context, in crucible.Request) (crucible.Response, error) {
	units := uint64(1)
	if raw, ok := in.Metadata["units"]; ok {
		if n, err := strconv.ParseUint(raw, 10, 64); err == nil && n >= 1 {
			units = n
		}
	}
	return crucible.Response{
		Payload:       map[string]any{"echo": in.Payload, "operation": in.Operation},
		BillableUnits: units,
	}, nil
}
