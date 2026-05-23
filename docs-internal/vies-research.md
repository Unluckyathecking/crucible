# VIES VAT Validation API — Research Findings (2026-05-23)

## REST API (Preferred)
- **Production**: `POST https://ec.europa.eu/taxation_customs/vies/rest-api/check-vat-number`
- **Test**: `POST https://ec.europa.eu/taxation_customs/vies/rest-api/check-vat-test-service`
- **Swagger**: `https://ec.europa.eu/assets/taxud/vow-information/swagger_publicVAT.yaml`

**Request**: `{"countryCode":"DE","vatNumber":"123456789"}`
**Response**: `{"countryCode":"DE","vatNumber":"...","requestDate":"...","valid":true,"name":"...","address":"..."}`

## Rate Limits & Caching (Redis)
| Result | TTL | Rationale |
|--------|-----|-----------|
| valid:true | 7 days | Registrations are stable |
| valid:false | 4 hours | User may fix typo |
| error/unavailable | 5 minutes | VIES recovers quickly |

**Cache key**: `vat:{country}:{number}` (uppercase, stripped)
**Safe throughput**: ~1 req/sec sustained

## Pre-Validation Regex (cheap reject before VIES)
| Country | Regex | Example |
|---------|-------|---------|
| DE | `^DE[0-9]{9}$` | DE123456789 |
| FR | `^FR[0-9A-Z]{2}[0-9]{9}$` | FRAB123456789 |
| IE | `^IE[0-9][A-Z0-9+*][0-9]{5}[A-Z]{1,2}$` | IE1234567A |
| NL | `^NL[0-9]{9}B[0-9]{2}$` | NL123456789B01 |
| ES | `^ES[A-Z0-9][0-9]{7}[A-Z0-9]$` | ESX1234567X |
| IT | `^IT[0-9]{11}$` | IT12345678901 |
| GB | `^GB[0-9]{9}$` or `^GB[0-9]{12}$` | GB123456789 |
| EL | `^EL[0-9]{9}$` | EL123456789 |
| AT | `^ATU[0-9]{8}$` | ATU12345678 |
| BE | `^BE[0-1][0-9]{9}$` | BE0123456789 |
| PL | `^PL[0-9]{10}$` | PL1234567890 |
| SE | `^SE[0-9]{12}$` | SE123456789001 |

## Design Decision
Use VIES **REST** API directly with `net/http` + `encoding/json`. No third-party library needed. Add Redis caching with state-dependent TTLs + per-country circuit breaker + exponential backoff retries (max 3).

## Common VIES Errors
| Error | Meaning | Action |
|-------|---------|--------|
| MS_UNAVAILABLE | Member state backend down | Retry with backoff |
| MS_MAX_CONCURRENT_REQ | Country rate limit | Backoff 5-10s |
| GLOBAL_MAX_CONCURRENT_REQ | Global overload | Backoff 10-30s |
| TIMEOUT | Request timed out | Retry once |
| INVALID_INPUT | Bad format | Reject immediately |
