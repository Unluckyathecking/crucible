package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// invokeWith posts a JSON invoke request to a handler via httptest and returns
// the decoded top-level response map.
func invokeWith(t *testing.T, h crucible.HandlerFunc, vatNumber string) map[string]interface{} {
	t.Helper()

	payload, _ := json.Marshal(map[string]string{"vat": vatNumber})
	reqBody, _ := json.Marshal(crucible.Request{
		RequestID:  "test-req",
		CustomerID: "cust-1",
		Operation:  "check",
		Payload:    payload,
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/invoke", func(w http.ResponseWriter, r *http.Request) {
		var req crucible.Request
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		resp, err := h(r.Context(), req)
		if err != nil {
			var serr *crucible.Error
			if asErr(err, &serr) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"error": serr})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"error": &crucible.Error{Code: "INTERNAL", Message: "internal error", Retryable: true}})
			return
		}
		if resp.BillableUnits == 0 {
			resp.BillableUnits = 1
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/invoke", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("invoke request: %v", err)
	}
	defer resp.Body.Close()

	var out map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return out
}

// asErr is a type assertion helper for *crucible.Error.
func asErr(err error, target **crucible.Error) bool {
	if err == nil {
		return false
	}
	if ce, ok := err.(*crucible.Error); ok {
		*target = ce
		return true
	}
	return false
}

// mockVIES returns an httptest.Server that always responds with the given status and body.
func mockVIES(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

// httpClientWithTimeout returns an http.Client with the given timeout.
func httpClientWithTimeout(d time.Duration) *http.Client {
	return &http.Client{Timeout: d}
}

// decodePayload unmarshals resp.Payload ([]byte / json.RawMessage) into a map.
func decodePayload(t *testing.T, resp crucible.Response) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	if err := json.Unmarshal(resp.Payload.([]byte), &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return m
}

// ----- Regex / format validation tests -----

func TestVatRegex_WellFormed(t *testing.T) {
	cases := []string{
		"DE123456789",
		"FR12123456789",
		"FRAB123456789",
		"IT12345678901",
		"GB123456789",
		"GB123456789012",
		"ATU12345678",
		"BE0123456789",
		"PL1234567890",
		"SE123456789012",
		"EL123456789",
		"NL123456789B01",
	}
	for _, raw := range cases {
		vat := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(raw, " ", ""), "-", ""))
		country := vat[:2]
		viesCountry := country
		if viesCountry == "GR" {
			viesCountry = "EL"
		}
		re, ok := vatRegex[viesCountry]
		if !ok {
			continue // not pre-validated
		}
		if !re.MatchString(vat) {
			t.Errorf("vatRegex[%s]: expected %q to match, but did not", viesCountry, vat)
		}
	}
}

func TestVatRegex_Malformed(t *testing.T) {
	cases := []struct {
		vat     string
		country string
	}{
		{"DE12345678", "DE"},    // 8 digits, need 9
		{"DE1234567890", "DE"},  // 10 digits, too long
		{"FR1123456789", "FR"},  // 2nd char must be [0-9A-Z]
		{"IT123456789", "IT"},   // 9 digits, need 11
		{"GB12345678", "GB"},    // 8 digits
		{"AT12345678", "AT"},    // missing U after AT
		{"BE2123456789", "BE"},  // 1st digit after BE must be 0 or 1
		{"PL123456789", "PL"},   // 9 digits, need 10
		{"SE12345678901", "SE"}, // 11 digits, need 12
		{"EL12345678", "EL"},    // 8 digits, need 9
		{"NL123456789B1", "NL"}, // trailing check must be 2 digits
	}
	for _, tc := range cases {
		vat := strings.ToUpper(strings.ReplaceAll(tc.vat, " ", ""))
		re, ok := vatRegex[tc.country]
		if !ok {
			t.Errorf("vatRegex: no entry for %s", tc.country)
			continue
		}
		if re.MatchString(vat) {
			t.Errorf("vatRegex[%s]: expected %q NOT to match", tc.country, vat)
		}
	}
}

// ----- Handler: input validation errors -----

func TestHandler_InvalidJSON(t *testing.T) {
	vies := mockVIES(t, 200, `{}`)
	defer vies.Close()
	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)

	_, err := h(context.Background(), crucible.Request{Payload: json.RawMessage(`not-json`)})
	if err == nil {
		t.Fatal("expected error for invalid JSON payload")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "BAD_REQUEST" {
		t.Errorf("code = %q, want BAD_REQUEST", serr.Code)
	}
	if serr.Retryable {
		t.Error("BAD_REQUEST must not be retryable")
	}
}

func TestHandler_VatTooShort(t *testing.T) {
	vies := mockVIES(t, 200, `{}`)
	defer vies.Close()
	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)

	payload, _ := json.Marshal(map[string]string{"vat": "AB"})
	_, err := h(context.Background(), crucible.Request{Payload: payload})
	if err == nil {
		t.Fatal("expected error for too-short VAT")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "BAD_REQUEST" {
		t.Errorf("code = %q, want BAD_REQUEST", serr.Code)
	}
}

func TestHandler_InvalidFormat_DE(t *testing.T) {
	vies := mockVIES(t, 200, `{}`)
	defer vies.Close()
	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)

	payload, _ := json.Marshal(map[string]string{"vat": "DE12345678"}) // 8 digits
	_, err := h(context.Background(), crucible.Request{Payload: payload})
	if err == nil {
		t.Fatal("expected INVALID_FORMAT error")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "INVALID_FORMAT" {
		t.Errorf("code = %q, want INVALID_FORMAT", serr.Code)
	}
	if serr.Retryable {
		t.Error("INVALID_FORMAT must not be retryable")
	}
}

func TestHandler_GRNormalizedToEL(t *testing.T) {
	// The handler converts GR → EL for the VIES country code.
	// Pass a GR-prefixed number so the normalization branch is actually exercised;
	// an EL-prefixed input would bypass the GR→EL code path entirely.
	var capturedBody map[string]string
	vies := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"countryCode":"EL","vatNumber":"123456789","valid":true}`))
	}))
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "GR123456789"})
	resp, err := h(context.Background(), crucible.Request{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.BillableUnits < 1 {
		t.Errorf("billable_units = %d, want >= 1", resp.BillableUnits)
	}
	// The normalization must have fired: VIES receives EL, not GR.
	if capturedBody["countryCode"] != "EL" {
		t.Errorf("VIES countryCode = %q, want EL (GR must be normalized to EL)", capturedBody["countryCode"])
	}
}

// ----- Handler: VIES upstream responses -----

func TestHandler_ViesValid(t *testing.T) {
	vies := mockVIES(t, 200, `{"countryCode":"DE","vatNumber":"123456789","valid":true,"name":"Acme GmbH","address":"Berlin"}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
	resp, err := h(context.Background(), crucible.Request{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.BillableUnits < 1 {
		t.Errorf("billable_units = %d, want >= 1", resp.BillableUnits)
	}
	if resp.UnitsLabel != "lookup" {
		t.Errorf("units_label = %q, want lookup", resp.UnitsLabel)
	}

	body := decodePayload(t, resp)
	if body["valid"] != true {
		t.Errorf("valid = %v, want true", body["valid"])
	}
	if body["country"] != "DE" {
		t.Errorf("country = %v, want DE", body["country"])
	}
	if body["name"] != "Acme GmbH" {
		t.Errorf("name = %v, want Acme GmbH", body["name"])
	}
	if body["address"] != "Berlin" {
		t.Errorf("address = %v, want Berlin", body["address"])
	}
}

func TestHandler_ViesInvalid(t *testing.T) {
	vies := mockVIES(t, 200, `{"countryCode":"DE","vatNumber":"999999999","valid":false}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE999999999"})
	resp, err := h(context.Background(), crucible.Request{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error for invalid-but-reachable VAT: %v", err)
	}
	// Unsuccessful lookup is still a billable event
	if resp.BillableUnits < 1 {
		t.Errorf("billable_units = %d, want >= 1", resp.BillableUnits)
	}

	body := decodePayload(t, resp)
	if body["valid"] != false {
		t.Errorf("valid = %v, want false", body["valid"])
	}
}

func TestHandler_ViesNoNameOrAddress(t *testing.T) {
	// When VIES omits name/address the keys must be absent from the response payload.
	vies := mockVIES(t, 200, `{"countryCode":"DE","vatNumber":"123456789","valid":true}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
	resp, err := h(context.Background(), crucible.Request{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	body := decodePayload(t, resp)
	if _, ok := body["name"]; ok {
		t.Error("name must be absent when VIES returns empty name")
	}
	if _, ok := body["address"]; ok {
		t.Error("address must be absent when VIES returns empty address")
	}
}

func TestHandler_Vies5xx(t *testing.T) {
	vies := mockVIES(t, 500, `{"fault":"internal server error"}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
	_, err := h(context.Background(), crucible.Request{Payload: payload})
	if err == nil {
		t.Fatal("expected error for VIES 5xx")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "VIES_ERROR" {
		t.Errorf("code = %q, want VIES_ERROR", serr.Code)
	}
	if !serr.Retryable {
		t.Error("VIES_ERROR must be retryable")
	}
}

func TestHandler_Vies503(t *testing.T) {
	vies := mockVIES(t, 503, `service unavailable`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
	_, err := h(context.Background(), crucible.Request{Payload: payload})
	if err == nil {
		t.Fatal("expected error for VIES 503")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "VIES_ERROR" {
		t.Errorf("code = %q, want VIES_ERROR", serr.Code)
	}
	if !serr.Retryable {
		t.Error("VIES 5xx must be retryable")
	}
}

func TestHandler_ViesTimeout(t *testing.T) {
	// Block the server handler on a channel so it never responds, causing the
	// client to time out deterministically. t.Cleanup unblocks the channel first,
	// then closes the server — order matters because vies.Close() waits for in-flight
	// connections to drain, and those connections block on <-unblock.
	unblock := make(chan struct{})
	vies := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-unblock // released by t.Cleanup before vies.Close()
	}))
	t.Cleanup(func() {
		close(unblock) // unblock the handler goroutine first
		vies.Close()   // then drain and shut down the server
	})

	// A very short client timeout guarantees the deadline fires in milliseconds,
	// not seconds, keeping the test fast even under -count/-race load.
	h := buildHandler(httpClientWithTimeout(20*time.Millisecond), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
	_, err := h(context.Background(), crucible.Request{Payload: payload})
	if err == nil {
		t.Fatal("expected error for VIES timeout")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "VIES_UNREACHABLE" {
		t.Errorf("code = %q, want VIES_UNREACHABLE", serr.Code)
	}
	if !serr.Retryable {
		t.Error("VIES_UNREACHABLE must be retryable")
	}
}

func TestHandler_ViesMalformedJSON(t *testing.T) {
	vies := mockVIES(t, 200, `not-valid-json`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
	_, err := h(context.Background(), crucible.Request{Payload: payload})
	if err == nil {
		t.Fatal("expected error for malformed VIES response")
	}
	var serr *crucible.Error
	if !asErr(err, &serr) {
		t.Fatalf("expected *crucible.Error, got %T", err)
	}
	if serr.Code != "VIES_PARSE_ERROR" {
		t.Errorf("code = %q, want VIES_PARSE_ERROR", serr.Code)
	}
	if !serr.Retryable {
		t.Error("VIES_PARSE_ERROR must be retryable")
	}
}

// ----- BillableUnits invariant -----

func TestHandler_BillableUnitsAlwaysAtLeastOne(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"valid", `{"countryCode":"DE","vatNumber":"123456789","valid":true}`},
		{"invalid", `{"countryCode":"DE","vatNumber":"123456789","valid":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vies := mockVIES(t, 200, tc.body)
			defer vies.Close()

			h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
			payload, _ := json.Marshal(map[string]string{"vat": "DE123456789"})
			resp, err := h(context.Background(), crucible.Request{Payload: payload})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if resp.BillableUnits < 1 {
				t.Errorf("billable_units = %d, want >= 1", resp.BillableUnits)
			}
		})
	}
}

// ----- Input normalisation -----

func TestHandler_NormalizesSpacesAndDashes(t *testing.T) {
	var capturedBody map[string]string
	vies := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"countryCode":"DE","vatNumber":"123456789","valid":true}`))
	}))
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "de 123-456-789"})
	resp, err := h(context.Background(), crucible.Request{Payload: payload})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.BillableUnits < 1 {
		t.Errorf("billable_units = %d, want >= 1", resp.BillableUnits)
	}
	if capturedBody["countryCode"] != "DE" {
		t.Errorf("VIES countryCode = %q, want DE", capturedBody["countryCode"])
	}
	if capturedBody["vatNumber"] != "123456789" {
		t.Errorf("VIES vatNumber = %q, want 123456789", capturedBody["vatNumber"])
	}
}

// ----- Unknown country (no regex entry) passes format check -----

func TestHandler_UnknownCountryPassesFormat(t *testing.T) {
	// Countries absent from vatRegex skip pre-validation and go straight to VIES.
	vies := mockVIES(t, 200, `{"countryCode":"CY","vatNumber":"12345678L","valid":true}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	payload, _ := json.Marshal(map[string]string{"vat": "CY12345678L"})
	resp, err := h(context.Background(), crucible.Request{Payload: payload})
	if err != nil {
		t.Fatalf("expected success for country without regex entry: %v", err)
	}
	if resp.BillableUnits < 1 {
		t.Errorf("billable_units = %d, want >= 1", resp.BillableUnits)
	}
}

// ----- End-to-end via invoke handler -----

func TestE2E_ValidVAT_Via_InvokeHandler(t *testing.T) {
	vies := mockVIES(t, 200, `{"countryCode":"IT","vatNumber":"12345678901","valid":true,"name":"Test SpA"}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	out := invokeWith(t, h, "IT12345678901")

	if _, hasError := out["error"]; hasError {
		t.Fatalf("unexpected error in response: %v", out["error"])
	}
	if out["billable_units"] == nil {
		t.Fatal("billable_units missing from response")
	}
	if bu := out["billable_units"].(float64); bu < 1 {
		t.Errorf("billable_units = %v, want >= 1", bu)
	}
}

func TestE2E_InvalidFormat_Via_InvokeHandler(t *testing.T) {
	vies := mockVIES(t, 200, `{}`)
	defer vies.Close()

	h := buildHandler(httpClientWithTimeout(5*time.Second), vies.URL)
	out := invokeWith(t, h, "IT123") // too short for IT

	if _, hasError := out["error"]; !hasError {
		t.Fatal("expected error in response for invalid format VAT")
	}
	errObj := out["error"].(map[string]interface{})
	if errObj["code"] != "INVALID_FORMAT" {
		t.Errorf("code = %v, want INVALID_FORMAT", errObj["code"])
	}
}
