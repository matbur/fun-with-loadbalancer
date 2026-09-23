package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
)

func TestServeHTTPForwardsRequest(t *testing.T) {
	var gotPath, gotHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHost = r.Header.Get("X-Forwarded-Host")
		fmt.Fprintln(w, "backend response")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/hello", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "/hello", gotPath)
	assert.Equal(t, "frontend.example", gotHost)
}

func TestServeHTTPEmptyPoolReturns503(t *testing.T) {
	srv := NewServer(balancer.NewPool(nil))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestServeHTTPUnhealthyBackendReturns503(t *testing.T) {
	b, err := balancer.NewBackend("http://127.0.0.1:1")
	require.NoError(t, err)
	b.Healthy.Store(false)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))
	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestServeHTTPBackendFailureReturns502(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}