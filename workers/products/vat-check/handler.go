package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// buildHandler constructs the VAT check handler using the provided HTTP client and
// VIES endpoint URL. Extracted from main() so tests can inject a mock VIES server.
func buildHandler(client *http.Client, viesEndpoint string) crucible.HandlerFunc {
	return func(ctx context.Context, req crucible.Request) (crucible.Response, error) {
		var payload struct {
			VAT string `json:"vat"`
		}
		if err := json.Unmarshal(req.Payload, &payload); err != nil {
			return crucible.Response{}, &crucible.Error{Code: "BAD_REQUEST", Message: "invalid payload: expected {\"vat\":\"...\"}", Retryable: false}
		}

		vat := strings.ToUpper(strings.ReplaceAll(strings.ReplaceAll(payload.VAT, " ", ""), "-", ""))
		if len(vat) < 3 {
			return crucible.Response{}, &crucible.Error{Code: "BAD_REQUEST", Message: "VAT number too short", Retryable: false}
		}

		country := vat[:2]
		number := vat[2:]

		// Normalize Greece: GR → EL for VIES
		viesCountry := country
		if viesCountry == "GR" {
			viesCountry = "EL"
		}

		// Pre-validation
		if re, ok := vatRegex[viesCountry]; ok {
			if !re.MatchString(vat) {
				return crucible.Response{}, &crucible.Error{Code: "INVALID_FORMAT", Message: fmt.Sprintf("VAT number does not match %s format", country), Retryable: false}
			}
		}

		// Call VIES REST API
		viesReq := map[string]string{
			"countryCode": viesCountry,
			"vatNumber":   number,
		}
		body, _ := json.Marshal(viesReq)

		viesResp, err := client.Post(viesEndpoint, "application/json", strings.NewReader(string(body)))
		if err != nil {
			return crucible.Response{}, &crucible.Error{Code: "VIES_UNREACHABLE", Message: "cannot reach VIES service", Retryable: true}
		}
		defer viesResp.Body.Close()

		if viesResp.StatusCode != http.StatusOK {
			return crucible.Response{}, &crucible.Error{Code: "VIES_ERROR", Message: fmt.Sprintf("VIES returned %d", viesResp.StatusCode), Retryable: true}
		}

		var result struct {
			CountryCode string `json:"countryCode"`
			VATNumber   string `json:"vatNumber"`
			Valid       bool   `json:"valid"`
			Name        string `json:"name"`
			Address     string `json:"address"`
		}
		if err := json.NewDecoder(viesResp.Body).Decode(&result); err != nil {
			return crucible.Response{}, &crucible.Error{Code: "VIES_PARSE_ERROR", Message: "invalid VIES response", Retryable: true}
		}

		respPayload := map[string]interface{}{
			"country": country,
			"valid":   result.Valid,
		}
		if result.Name != "" {
			respPayload["name"] = result.Name
		}
		if result.Address != "" {
			respPayload["address"] = result.Address
		}

		payloadBytes, _ := json.Marshal(respPayload)
		return crucible.Response{
			Payload:       payloadBytes,
			BillableUnits: 1,
			UnitsLabel:    "lookup",
		}, nil
	}
}
