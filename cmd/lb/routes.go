package main

import (
	"maps"
	"net/http"
	"slices"
	"strings"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/metrics"
)

// methodRouter dispatches on the request method and answers 405 with an Allow
// header for every other method.
type methodRouter map[string]http.HandlerFunc

func (r methodRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if h, ok := r[req.Method]; ok {
		h(w, req)
		return
	}
	w.Header().Set("Allow", strings.Join(slices.Sorted(maps.Keys(r)), ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// newMux builds the routes the load balancer serves: the admin API, the
// orchestrator probes, the metrics endpoint, and the proxy as the catch-all.
//
// The reserved paths are registered per path with explicit method dispatch
// rather than with method-scoped patterns ("GET /backends"). A method mismatch
// only turns into a 405 while no other pattern matches the path, and the proxy
// is registered as "/", which matches every path — so with method-scoped
// patterns "PUT /backends" would fall through to the proxy and be answered by
// an application backend instead of being rejected.
func newMux(pool *balancer.Pool, admin *adminAPI, m *metrics.Metrics, proxied http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	// HEAD sits next to GET on every path because the method-scoped "GET"
	// patterns it replaces also matched HEAD.
	mux.Handle("/backends", methodRouter{
		http.MethodGet:    admin.list,
		http.MethodHead:   admin.list,
		http.MethodPost:   admin.add,
		http.MethodDelete: admin.remove,
	})
	mux.Handle("/live", methodRouter{
		http.MethodGet:  handleLive,
		http.MethodHead: handleLive,
	})
	mux.Handle("/ready", methodRouter{
		http.MethodGet:  handleReady(pool),
		http.MethodHead: handleReady(pool),
	})
	mux.Handle("/", proxied)
	return mux
}
