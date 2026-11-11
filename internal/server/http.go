// Package server contains Mosso's HTTP API.
package server

import (
	"encoding/json"
	"net/http"
)

// NewHandler returns the initial HTTP handler. Do not expose the server publicly
// until authentication and authorization have been implemented.
func NewHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	return mux
}
