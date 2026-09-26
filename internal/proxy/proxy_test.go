package proxy

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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

// abortingBackend starts a backend that promises more body than it sends and
// then drops the connection, so the proxy's response copy fails mid-body. The
// returned counter counts the requests it actually received.
func abortingBackend(t *testing.T) (string, *atomic.Int32) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { ln.Close() })

	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				req := make([]byte, 4096)
				_, _ = c.Read(req)
				hits.Add(1)
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\npartial"))
			}(c)
		}
	}()
	return "http://" + ln.Addr().String(), &hits
}

// TestServeHTTPReleasesInFlightSlotOnAbortedResponse pins the in-flight
// accounting when the response copy aborts. ReverseProxy panics with
// http.ErrAbortHandler in that case, so the slot taken by Acquire has to be
// released on the panic path too — otherwise every aborted GET/HEAD/OPTIONS
// inflates the counter for the process lifetime and permanently skews
// least-connections selection.
func TestServeHTTPReleasesInFlightSlotOnAbortedResponse(t *testing.T) {
	backendURL, hits := abortingBackend(t)
	b, err := balancer.NewBackend(backendURL)
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})

	// A real server, not a recorder: ReverseProxy only raises the
	// ErrAbortHandler panic for requests that came through net/http's server,
	// which is also what recovers it in production.
	lb := httptest.NewServer(NewServer(pool))
	defer lb.Close()

	for _, method := range []string{http.MethodGet, http.MethodPost} {
		before := hits.Load()

		conn, err := net.Dial("tcp", lb.Listener.Addr().String())
		require.NoError(t, err)
		_, err = fmt.Fprintf(conn, "%s / HTTP/1.1\r\nHost: lb\r\nContent-Length: 0\r\n\r\n", method)
		require.NoError(t, err)
		// The aborted copy leaves no complete response, so the load balancer
		// drops the connection. Reading until EOF also means the handler has
		// finished unwinding.
		_, _ = io.Copy(io.Discard, conn)
		require.NoError(t, conn.Close())

		require.Eventually(t, func() bool { return hits.Load() > before },
			time.Second, 10*time.Millisecond,
			"the aborted %s must have reached the backend for this test to mean anything", method)
		assert.Eventually(t, func() bool { return b.Active() == 0 },
			time.Second, 10*time.Millisecond,
			"aborted %s must not leave an in-flight slot behind", method)
	}
	assert.Zero(t, b.Active())
}

func TestServeHTTPCircuitBreakerSkipsFailingBackend(t *testing.T) {
	// Backend A is dead: every routing attempt ends in a dial error that
	// feeds its circuit breaker (threshold 1 → opens on the first failure).
	deadA := httptest.NewServer(http.NotFoundHandler())
	deadAURL := deadA.URL
	deadA.Close()

	var countB atomic.Int32
	backendB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		countB.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backendB.Close()

	bA, err := balancer.NewBackend(deadAURL, balancer.WithBreakerThreshold(1))
	require.NoError(t, err)
	bB, err := balancer.NewBackend(backendB.URL)
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{bA, bB}))

	// Round-robin guarantees A is reached within these requests; the
	// retryable GETs fall back to B.
	for range 3 {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	require.False(t, bA.Available(), "backend A's circuit must be open after the failures")

	before := countB.Load()
	for range 5 {
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
		assert.Equal(t, http.StatusOK, rec.Code)
	}
	assert.Equal(t, int32(5), countB.Load()-before,
		"open circuit: every request must be served by B in a single attempt")
}

// recordHandler is a minimal slog.Handler that collects records in memory,
// so tests can assert on the structured access logs the server emits.
type recordHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *recordHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *recordHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}
func (h *recordHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *recordHandler) WithGroup(string) slog.Handler       { return h }

func recordAttrs(r slog.Record) map[string]any {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	return attrs
}

func TestServeHTTPLogsSuccessfulRequest(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)

	collector := &recordHandler{}
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithLogger(slog.New(collector)))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/hello", nil))

	require.Len(t, collector.records, 1, "one access log line per request")
	assert.Equal(t, "request", collector.records[0].Message)
	assert.Equal(t, slog.LevelInfo, collector.records[0].Level)

	attrs := recordAttrs(collector.records[0])
	assert.Equal(t, "GET", attrs["method"])
	assert.Equal(t, "/hello", attrs["path"])
	assert.Equal(t, int64(200), attrs["status"])
	assert.Equal(t, b.URL.Host, attrs["backend"])
	_, ok := attrs["duration"].(time.Duration)
	assert.True(t, ok, "duration must be logged as a duration")
}

func TestServeHTTPWithoutLoggerStaysSilent(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)

	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)
}

func TestServeHTTPLogsNoBackendsAsWarn(t *testing.T) {
	collector := &recordHandler{}
	srv := NewServer(balancer.NewPool(nil), WithLogger(slog.New(collector)))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))

	require.Len(t, collector.records, 1)
	assert.Equal(t, slog.LevelWarn, collector.records[0].Level)
	assert.Equal(t, int64(http.StatusServiceUnavailable), recordAttrs(collector.records[0])["status"])
}

func TestServeHTTPLogsBackend5xxAsWarn(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)

	collector := &recordHandler{}
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithLogger(slog.New(collector)))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))

	require.Len(t, collector.records, 1)
	assert.Equal(t, slog.LevelWarn, collector.records[0].Level)
	assert.Equal(t, int64(http.StatusInternalServerError), recordAttrs(collector.records[0])["status"])
	assert.Equal(t, b.URL.Host, recordAttrs(collector.records[0])["backend"])
}

func TestServeHTTPBodyLimitContentLength(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMaxBodyBytes(1024))

	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/",
		strings.NewReader(strings.Repeat("x", 2048)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code,
		"declared Content-Length over the limit must be rejected with 413")
}

func TestServeHTTPBodyLimitChunked(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMaxBodyBytes(1024))

	// Chunked transfer: unknown length, so the limit must be enforced while
	// streaming the body (http.MaxBytesReader) and map to 413, not 502.
	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/",
		strings.NewReader(strings.Repeat("x", 4096)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

func TestServeHTTPForwardsBodyWithinLimit(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%d", len(n))
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMaxBodyBytes(1024))

	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/",
		strings.NewReader(strings.Repeat("x", 512)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "512", rec.Body.String(), "the whole body within the limit must be forwarded")
}

func TestServeHTTPNoLimitsByDefault(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, _ := io.ReadAll(r.Body)
		fmt.Fprintf(w, "%d", len(n))
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}))

	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/",
		strings.NewReader(strings.Repeat("x", 2<<20)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "2097152", rec.Body.String(), "without a configured limit big bodies pass through")
}

func TestServeHTTPBodyTooLargeDoesNotTripBreaker(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL,
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(30*time.Second),
	)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMaxBodyBytes(1024))

	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/",
		strings.NewReader(strings.Repeat("x", 4096)))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)

	// The 413 is client-caused: it must not count as a backend failure, so
	// with threshold 1 the circuit stays closed and the next request succeeds.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "body-too-large must not trip the circuit breaker")
}

func TestServeHTTPConcurrencyLimit(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMaxConcurrent(1))

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	}()
	<-entered // first request holds the only slot and is blocked in the backend

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code,
		"the second request must be rejected while the server is at capacity")

	close(release)
	<-firstDone // first request completes
}

func TestServeHTTPConcurrentWithinLimit(t *testing.T) {
	var started atomic.Int32
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started.Add(1)
		<-release
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMaxConcurrent(2))

	done := make(chan struct{}, 2)
	for range 2 {
		go func() {
			defer func() { done <- struct{}{} }()
			srv.ServeHTTP(httptest.NewRecorder(),
				httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
		}()
	}
	// Both must be running inside the backend at once (limit of 2 respected).
	for range 2 {
		for started.Load() < 2 {
			time.Sleep(time.Millisecond)
		}
	}
	close(release)
	for range 2 {
		<-done
	}
}

func TestServeHTTPConcurrencyLimitLogsWarn(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, "ok")
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)
	collector := &recordHandler{}
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}),
		WithMaxConcurrent(1), WithLogger(slog.New(collector)))

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		srv.ServeHTTP(httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	}()
	<-entered // first request holds the only slot

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)

	// The first request is still blocked, so the rejection warn is the only
	// record so far — the slot release happens only after we finish asserting.
	require.Len(t, collector.records, 1)
	assert.Equal(t, slog.LevelWarn, collector.records[0].Level)
	assert.Equal(t, int64(http.StatusServiceUnavailable), recordAttrs(collector.records[0])["status"])

	close(release)
	<-firstDone
}
