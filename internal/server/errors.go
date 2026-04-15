package server

import (
	"encoding/json"
	"net/http"
)

// jsonError writes a JSON error response for /api/v1/ endpoints.
func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
