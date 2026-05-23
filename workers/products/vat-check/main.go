package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	crucible "github.com/Unluckyathecking/crucible/workers/sdk-go"
)

// Pre-validation regex for supported countries
var vatRegex = map[string]*regexp.Regexp{
	"DE": regexp.MustCompile(`^DE[0-9]{9}$`),
	"FR": regexp.MustCompile(`^FR[0-9A-Z]{2}[0-9]{9}$`),
	"IE": regexp.MustCompile(`^IE[0-9][A-Z0-9+*][0-9]{5}[A-Z]{1,2}$`),
	"NL": regexp.MustCompile(`^NL[0-9]{9}B[0-9]{2}$`),
	"ES": regexp.MustCompile(`^ES[A-Z0-9][0-9]{7}[A-Z0-9]$`),
	"IT": regexp.MustCompile(`^IT[0-9]{11}$`),
	"GB": regexp.MustCompile(`^GB[0-9]{9}$|^GB[0-9]{12}$`),
	"EL": regexp.MustCompile(`^EL[0-9]{9}$`),
	"AT": regexp.MustCompile(`^ATU[0-9]{8}$`),
	"BE": regexp.MustCompile(`^BE[0-1][0-9]{9}$`),
	"PL": regexp.MustCompile(`^PL[0-9]{10}$`),
	"SE": regexp.MustCompile(`^SE[0-9]{12}$`),
}

func main() {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	crucible.Serve(8081, func(ctx context.Context, req crucible.Request) (crucible.Response, error) {
		// Parse payload: {"vat": "DE123456789"}
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

		viesResp, err := httpClient.Post(
			"https://ec.europa.eu/taxation_customs/vies/rest-api/check-vat-number",
			"application/json",
			strings.NewReader(string(body)),
		)
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
	})
}
