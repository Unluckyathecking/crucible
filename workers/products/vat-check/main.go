package main

import (
	"net/http"
	"regexp"
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

const viesEndpoint = "https://ec.europa.eu/taxation_customs/vies/rest-api/check-vat-number"

func main() {
	client := &http.Client{Timeout: 10 * time.Second}
	crucible.Serve(8081, buildHandler(client, viesEndpoint))
}
