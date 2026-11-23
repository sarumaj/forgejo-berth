// SPDX-License-Identifier: GPL-3.0-or-later

// Package web serves the broker's metrics, state and health over HTTP.
package web

import (
	"encoding/json"
	"net/http"
	"time"

	"git.ciepiela.org/dawid/forgejo-berth/internal/broker"
)

// Source is what the endpoints read.
type Source interface {
	Snapshot() broker.State
	Healthy(now time.Time) bool
}

// Handler serves /metrics (Prometheus' text format), /state (JSON) and /healthz.
func Handler(src Source) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		_, _ = w.Write([]byte(src.Snapshot().Metrics()))
	})
	mux.HandleFunc("GET /state", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(src.Snapshot())
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !src.Healthy(time.Now()) {
			http.Error(w, "no recent look", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}
