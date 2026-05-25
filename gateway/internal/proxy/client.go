// Package proxy forwards Invoke() calls from the gateway to a worker over HTTP/JSON.
// The wire encoding matches gateway/proto/tool.proto. gRPC mode is a future swap;
// this layer is the seam where transport choice lives.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Unluckyathecking/crucible/gateway/internal/observability"
)

// InvokeRequest mirrors the proto for HTTP/JSON wire encoding.
type InvokeRequest struct {
	RequestID  string            `json:"request_id"`
	CustomerID string            `json:"customer_id"`
	Operation  string            `json:"operation"`
	Payload    json.RawMessage   `json:"payload"`
	Plan       string            `json:"plan"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

// InvokeResponse mirrors the proto. Either Payload or Error is set per call.
type InvokeResponse struct {
	Payload       json.RawMessage `json:"payload,omitempty"`
	Error         *InvokeError    `json:"error,omitempty"`
	BillableUnits uint64          `json:"billable_units"`
	UnitsLabel    string          `json:"units_label,omitempty"`
}

type InvokeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

// Client forwards InvokeRequests to a worker.
type Client struct {
	workerURL string
	http      *http.Client
}

func New(workerURL string, timeout time.Duration) *Client {
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		workerURL: workerURL,
		http: &http.Client{
			Timeout: timeout,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 50,
				IdleConnTimeout:     90 * time.Second,
			},
		},
	}
}

// Invoke POSTs an InvokeRequest to the worker's /invoke endpoint and decodes the response.
// Returns a transport error if the network call fails or the worker returns an unexpected shape.
// Returns a successful (*InvokeResponse, nil) if the worker returned a structured error envelope.
func (c *Client) Invoke(ctx context.Context, in *InvokeRequest) (*InvokeResponse, error) {
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.workerURL+"/invoke", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if in.RequestID != "" {
		req.Header.Set("X-Request-ID", in.RequestID)
	}

	start := time.Now()
	resp, err := c.http.Do(req)
	observability.WorkerCallDuration.Observe(time.Since(start).Seconds())
	if err != nil {
		return nil, fmt.Errorf("worker call: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		// Surface up to 512 bytes of the body in the error — invaluable when triaging
		// a misbehaving worker. Customer-facing errors are still sanitised at the route
		// handler; this body only lands in the gateway's own structured logs.
		bodyPeek, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		// Drain anything past the peek so the connection can be reused from the pool.
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil, fmt.Errorf("worker returned status %d: %s",
			resp.StatusCode, strings.TrimSpace(string(bodyPeek)))
	}

	var out InvokeResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode worker response: %w", err)
	}
	if out.Payload == nil && out.Error == nil {
		return nil, errors.New("worker returned neither payload nor error")
	}
	return &out, nil
}
