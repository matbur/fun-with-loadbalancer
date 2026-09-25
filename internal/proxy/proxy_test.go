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
	for range total {
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

// httputil.ReverseProxy strips hop-by-hop headers (RFC 7230 §6.1) in both
// directions. These tests pin that behavior so it cannot regress silently.

func TestServeHTTPStripsHopByHopHeadersFromRequest(t *testing.T) {
	var got http.Header
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	req.Header.Set("Connection", "X-Hop")
	req.Header.Set("X-Hop", "secret") // listed in Connection → stripped too
	req.Header.Set("Keep-Alive", "timeout=5")
	req.Header.Set("Te", "deflate") // "TE: trailers" is re-added by stdlib by design; use a non-trailers value
	req.Header.Set("Transfer-Encoding", "chunked")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Proxy-Connection", "keep-alive")
	req.Header.Set("Proxy-Authorization", "Basic abc")
	req.Header.Set("X-Normal", "keepme")

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	for _, h := range []string{
		"Connection", "X-Hop", "Keep-Alive", "Te",
		"Transfer-Encoding", "Upgrade", "Proxy-Connection", "Proxy-Authorization",
	} {
		assert.Empty(t, got.Get(h), "hop-by-hop header %s must not reach the backend", h)
	}
	assert.Equal(t, "keepme", got.Get("X-Normal"))
}

func TestServeHTTPStripsHopByHopHeadersFromResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Connection", "X-Backend")
		h.Set("X-Backend", "secret") // listed in Connection → stripped too
		h.Set("Keep-Alive", "timeout=5")
		h.Set("Upgrade", "websocket")
		h.Set("Te", "gzip")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	for _, h := range []string{"Connection", "X-Backend", "Keep-Alive", "Upgrade", "Te"} {
		assert.Empty(t, rec.Header().Get(h), "hop-by-hop header %s must not reach the client", h)
	}
}

func TestServeHTTPDoesNotForwardConnectionClose(t *testing.T) {
	var got string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Connection")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	req.Header.Set("Connection", "close")
	req.Close = true // client asked to close after this request

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Empty(t, got, "client Connection: close must not reach the backend (proxy keeps its own connection)")
}

func TestServeHTTPLeastConnections(t *testing.T) {
	release := make(chan struct{})
	blocking := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer blocking.Close()

	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer fast.Close()

	bBlock, err := balancer.NewBackend(blocking.URL)
	require.NoError(t, err)
	bFast, err := balancer.NewBackend(fast.URL)
	require.NoError(t, err)

	pool := balancer.NewPool(
		[]*balancer.Backend{bBlock, bFast},
		balancer.WithAlgorithm(balancer.AlgorithmLeastConnections),
	)
	srv := NewServer(pool)

	// First request blocks on the first backend in the list.
	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	}()

	// Wait until the blocking backend actually carries the in-flight request.
	require.Eventually(t, func() bool { return bBlock.Active() == 1 }, time.Second, 10*time.Millisecond)

	// Second request must go to the idle backend instead of blocking.
	secondDone := make(chan struct{})
	rec := httptest.NewRecorder()
	go func() {
		defer close(secondDone)
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	}()

	select {
	case <-secondDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second request blocked — least-connections did not pick the idle backend")
	}
	assert.Equal(t, http.StatusOK, rec.Code)

	close(release)
	<-firstDone
}
