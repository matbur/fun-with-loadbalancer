package main

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"fun-with-loadbalancer/internal/balancer"
)

// adminAPI implements the /backends endpoint for dynamic backend management:
//
//	GET    /backends            → JSON list of backends
//	POST   /backends            → add a backend   {"url": "...", "weight": 2}
//	DELETE /backends            → remove a backend {"url": "..."}
//
// It is unauthenticated by design — intended for local/development use.
type adminAPI struct {
	pool             *balancer.Pool
	transport        http.RoundTripper
	breakerThreshold int
	breakerCooldown  time.Duration
}

func newAdminAPI(pool *balancer.Pool, transport http.RoundTripper, breakerThreshold int, breakerCooldown time.Duration) *adminAPI {
	return &adminAPI{
		pool:             pool,
		transport:        transport,
		breakerThreshold: breakerThreshold,
		breakerCooldown:  breakerCooldown,
	}
}

type backendInfo struct {
	URL     string `json:"url"`
	Weight  int    `json:"weight"`
	Healthy bool   `json:"healthy"`
	Active  int    `json:"active"`
}

func (a *adminAPI) list(w http.ResponseWriter, r *http.Request) {
	backends := a.pool.Backends()
	infos := make([]backendInfo, 0, len(backends))
	for _, b := range backends {
		infos = append(infos, backendInfo{
			URL:     b.URL.String(),
			Weight:  b.Weight(),
			Healthy: b.Healthy.Load(),
			Active:  b.Active(),
		})
	}
	writeJSON(w, http.StatusOK, infos)
}

type backendRequest struct {
	URL    string `json:"url"`
	Weight int    `json:"weight"`
}

func (a *adminAPI) add(w http.ResponseWriter, r *http.Request) {
	var req backendRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.URL == "" {
		http.Error(w, "url is required", http.StatusBadRequest)
		return
	}

	// The pool identifies backends by pointer; the admin API works by URL,
	// so URL uniqueness is enforced here.
	for _, existing := range a.pool.Backends() {
		if existing.URL.String() == req.URL {
			http.Error(w, "backend already in pool", http.StatusConflict)
			return
		}
	}

	b, err := balancer.NewBackend(req.URL,
		balancer.WithTransport(a.transport),
		balancer.WithWeight(req.Weight), // clamps to >= 1, so an omitted weight means 1
		balancer.WithBreakerThreshold(a.breakerThreshold),
		balancer.WithBreakerCooldown(a.breakerCooldown),
	)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if err := a.pool.AddBackend(b); err != nil {
		if errors.Is(err, balancer.ErrDuplicateBackend) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusCreated)
}

func (a *adminAPI) remove(w http.ResponseWriter, r *http.Request) {
	var req backendRequest
	if err := decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid JSON body: "+err.Error(), http.StatusBadRequest)
		return
	}

	for _, b := range a.pool.Backends() {
		if b.URL.String() == req.URL {
			if err := a.pool.RemoveBackend(b); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	http.Error(w, "backend not found", http.StatusNotFound)
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}