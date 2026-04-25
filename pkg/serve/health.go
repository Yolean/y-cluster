package serve

import (
	"encoding/json"
	"net/http"
)

// HealthHandler returns 200 with a small JSON payload describing the
// backend. Ensure probes this on every configured port before returning.
// `extra` is snapshotted once; for backends whose payload changes over
// time (e.g. watch-driven route counts), use HealthHandlerFunc.
func HealthHandler(kind BackendType, extra map[string]any) http.HandlerFunc {
	snap := make(map[string]any, len(extra))
	for k, v := range extra {
		snap[k] = v
	}
	return HealthHandlerFunc(kind, func() map[string]any { return snap })
}

// HealthHandlerFunc is like HealthHandler but invokes the provider on
// every request, so watch-driven backends can report the current
// number of routes/namespace/etc. The provider may be nil, in which
// case only {ok, type} is returned.
func HealthHandlerFunc(kind BackendType, extra func() map[string]any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			MethodNotAllowed(w, http.MethodGet, http.MethodHead)
			return
		}
		payload := map[string]any{
			"ok":   true,
			"type": string(kind),
		}
		if extra != nil {
			for k, v := range extra() {
				payload[k] = v
			}
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
