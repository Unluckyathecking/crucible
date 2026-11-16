package httputil

import (
	"encoding/json"
	"net/http"
)

// WriteJSON encodes v as the JSON response body with Content-Type
// application/json and Cache-Control no-store. The status is left to the
// caller (default 200).
func WriteJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
