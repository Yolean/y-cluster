package serve

import (
	"encoding/json"
	"net/http"
)

// HealthHandler returns 200 with a small JSON payload describing the
// backend. Ensure probes this on every configured port before returning.
func HealthHandler(kind BackendType, extra map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			MethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		payload := map[string]any{
			"ok":   true,
			"type": string(kind),
		}
		for k, v := range extra {
			payload[k] = v
		}
		body, _ := json.Marshal(payload)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = w.Write(body)
	}
}
