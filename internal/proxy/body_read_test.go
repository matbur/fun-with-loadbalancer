package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/metrics"
)

// errClientGone stands in for a client that dies mid-upload.
var errClientGone = errors.New("read tcp 10.0.0.1:5678->10.0.0.2:8080: connection reset by peer")

// failingBody yields one chunk and then fails, the shape a client that hangs up
// or blows a read deadline mid-transfer produces.
type failingBody struct {
	err  error
	sent bool
}

func (f *failingBody) Read(p []byte) (int, error) {
	if f.sent {
		return 0, f.err
	}
	f.sent = true
	return copy(p, "partial"), nil
}

func (f *failingBody) Close() error { return nil }

// newFailingBodyRequest builds a chunked request whose body dies after one chunk.
func newFailingBodyRequest(method string, readErr error) *http.Request {
	req := httptest.NewRequest(method, "http://frontend.example/", &failingBody{err: readErr})
	req.ContentLength = -1
	return req
}

// newBodyReadFailureServer wires one healthy backend behind a pool that trips on
// the first reported failure, so any failure reaching the breaker shows up, plus
// the metrics that count attempts and per-backend transport errors.
func newBodyReadFailureServer(t *testing.T) (*Server, *metrics.Metrics, string) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		fmt.Fprint(w, "ok")
	}))
	t.Cleanup(backend.Close)

	b, err := balancer.NewBackend(backend.URL,
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(30*time.Second))
	require.NoError(t, err)

	m := metrics.New()
	return NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMetrics(m)), m, b.URL.Host
}

// TestServeHTTPBodyReadFailureDoesNotTripBreaker is the regression test for
// review finding 9. ReverseProxy copies the request body to the backend, so a
// body that never arrives surfaces as an upstream transport error — the same
// shape as a broken backend. The proxy charged it to the backend, so a client
// that gave up mid-upload got a 502 and opened the circuit of a backend that was
// answering fine. With one such client per backend that takes the whole
// balancer out: /ready starts reporting 503 and the orchestrator drops it.
func TestServeHTTPBodyReadFailureDoesNotTripBreaker(t *testing.T) {
	srv, m, host := newBodyReadFailureServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newFailingBodyRequest(http.MethodPost, errClientGone))

	assert.Equal(t, http.StatusBadRequest, rec.Code,
		"a body that never arrived is the client's failure, not a 502")
	assert.Zero(t, m.BackendErrors(host), "a failed body read is not a backend transport error")

	// Threshold is 1, so any reported failure would have opened the circuit.
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "a client's failed upload must not trip the breaker")
}

// TestServeHTTPBodyReadTimeoutReturns408 separates the timeout case: a read
// deadline expiring mid-transfer is 408, not 400, so a client can tell "you were
// too slow" from "your request was malformed".
func TestServeHTTPBodyReadTimeoutReturns408(t *testing.T) {
	srv, m, host := newBodyReadFailureServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newFailingBodyRequest(http.MethodPost, context.DeadlineExceeded))

	assert.Equal(t, http.StatusRequestTimeout, rec.Code)
	assert.Zero(t, m.BackendErrors(host))
}

// TestServeHTTPRetriesAreNotSpentOnAFailedBody pins the other half. A GET is
// retryable, but a second attempt cannot succeed — the body is already gone
// from the client connection, so the retry would forward a truncated request and
// charge the backend for the failure as well.
func TestServeHTTPRetriesAreNotSpentOnAFailedBody(t *testing.T) {
	srv, m, host := newBodyReadFailureServer(t)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newFailingBodyRequest(http.MethodGet, errClientGone))

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Equal(t, float64(1), m.BackendRequests(host),
		"a client-caused failure must not spend the retry")
}

// TestServeHTTPEOFIsNotAReadFailure guards the one case the body wrapper must
// not catch: a body that simply ends is how requests normally finish.
func TestServeHTTPEOFIsNotAReadFailure(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.ReadAll(r.Body)
		assert.NoError(t, err)
		fmt.Fprintf(w, "%d", len(n))
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL,
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(30*time.Second))
	require.NoError(t, err)
	m := metrics.New()
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMetrics(m))

	req := httptest.NewRequest(http.MethodPost, "http://frontend.example/", strings.NewReader("hello"))
	req.ContentLength = -1
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "5", rec.Body.String(), "a body ending in EOF must be forwarded whole")
	assert.Zero(t, m.BackendErrors(b.URL.Host))
}

// TestServeHTTPBackendFailureStillTripsBreaker is the other side of the new
// branch: an error that is not a body read failure must still be charged to the
// backend, or the wrapper has swallowed real failures too.
func TestServeHTTPBackendFailureStillTripsBreaker(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	backend.Close() // nothing is listening any more

	b, err := balancer.NewBackend(backend.URL,
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(30*time.Second))
	require.NoError(t, err)
	m := metrics.New()
	srv := NewServer(balancer.NewPool([]*balancer.Backend{b}), WithMetrics(m))

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "http://frontend.example/",
		strings.NewReader("hello")))

	assert.Equal(t, http.StatusBadGateway, rec.Code)
	assert.Equal(t, float64(1), m.BackendErrors(b.URL.Host), "a dead backend is still a backend error")
}
