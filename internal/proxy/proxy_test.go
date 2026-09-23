package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

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

func TestServeHTTPRoundRobinsRequests(t *testing.T) {
	var count1, count2 atomic.Int32

	backend1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count1.Add(1)
	}))
	defer backend1.Close()

	backend2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count2.Add(1)
	}))
	defer backend2.Close()

	b1, err := balancer.NewBackend(backend1.URL)
	require.NoError(t, err)
	b2, err := balancer.NewBackend(backend2.URL)
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{b1, b2}))

	const total = 10
	for i := 0; i < total; i++ {
		req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code)
	}

	assert.Equal(t, int32(total/2), count1.Load())
	assert.Equal(t, int32(total/2), count2.Load())
}

func TestServeHTTPBackendHeaderTimeoutReturns502(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
	}))
	defer backend.Close()

	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: time.Second}).DialContext,
		ResponseHeaderTimeout: 50 * time.Millisecond,
	}
	b, err := balancer.NewBackend(backend.URL, balancer.WithTransport(tr))
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Less(t, time.Since(start), 400*time.Millisecond)
}
