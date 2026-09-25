package proxy

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/metrics"
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

func TestServeHTTPBackendHeaderTimeoutReturns504(t *testing.T) {
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

	assert.Equal(t, http.StatusGatewayTimeout, rec.Code)
	assert.Less(t, time.Since(start), 400*time.Millisecond)
}

func TestServeHTTPRetriesIdempotentRequest(t *testing.T) {
	var goodCount atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCount.Add(1)
		fmt.Fprint(w, "good")
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bad.Close()

	bGood, err := balancer.NewBackend(good.URL)
	require.NoError(t, err)
	bBad, err := balancer.NewBackend(bad.URL)
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{bGood, bBad}))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, int32(1), goodCount.Load())
}

func TestServeHTTPDoesNotRetryPOST(t *testing.T) {
	var goodCount atomic.Int32
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodCount.Add(1)
	}))
	defer good.Close()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	bad.Close()

	bGood, err := balancer.NewBackend(good.URL)
	require.NoError(t, err)
	bBad, err := balancer.NewBackend(bad.URL)
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{bGood, bBad}))

	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, int32(0), goodCount.Load())
}

func TestServeHTTPPassesThroughUpstream500(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusInternalServerError, rec.Code)
}

func TestServeHTTPConcurrentRequests(t *testing.T) {
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

	const goroutines = 20
	const requestsPerGoroutine = 10
	const total = goroutines * requestsPerGoroutine

	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range requestsPerGoroutine {
				req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
				rec := httptest.NewRecorder()
				srv.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("got status %d, want 200", rec.Code)
					return
				}
			}
		})
	}
	wg.Wait()

	assert.Equal(t, int32(total/2), count1.Load())
	assert.Equal(t, int32(total/2), count2.Load())
}

func TestServeHTTPWithMetricsCountsBackendRequests(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)

	m := metrics.New()
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMetrics(m))

	for range 3 {
		req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)
	}

	assert.Equal(t, 3.0, m.BackendRequests(b.URL.Host))
}

func TestServeHTTPWithMetricsCountsBackendErrors(t *testing.T) {
	// Start a backend, then take it down: dialing it will fail with
	// connection refused.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := backend.URL
	backend.Close()

	b, err := balancer.NewBackend(url)
	require.NoError(t, err)

	m := metrics.New()
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMetrics(m))

	// GET is retryable → 2 attempts, both fail → 502.
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, 2.0, m.BackendErrors(b.URL.Host))
	assert.Equal(t, 2.0, m.BackendRequests(b.URL.Host))

	// POST is not retryable → 1 attempt → 502.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, 3.0, m.BackendErrors(b.URL.Host))
	assert.Equal(t, 3.0, m.BackendRequests(b.URL.Host))
}
