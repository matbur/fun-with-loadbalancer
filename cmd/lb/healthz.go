package main

import (
	"net/http"

	"fun-with-loadbalancer/internal/balancer"
)

// handleLive implements GET /live — process liveness for orchestrators
// (Kubernetes liveness probe). It returns 200 for as long as the process
// runs; there is no internal state to check beyond that.
func handleLive(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// handleReady implements GET /ready — readiness to serve traffic. It returns
// 200 when at least one backend can take traffic (pool.Ready), 503 otherwise
// — the same outcome the proxy reports as "no healthy backends".
func handleReady(pool *balancer.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if pool.Ready() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
	}
}