
## 2026-05-25 - Replace fmt.Sprintf with strconv in hot paths
**Learning:** Using `fmt.Sprintf` for simple integer to string conversions on HTTP server hot paths causes unnecessary allocations and reflection overhead.
**Action:** Use `strconv.FormatUint` or `strconv.Itoa` depending on the type when converting integers to strings, especially for logging or HTTP headers.
