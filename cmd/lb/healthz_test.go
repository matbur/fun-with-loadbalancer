package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/metrics"
)

func newHealthzServer(t *testing.T) (*httptest.Server, *balancer.Pool) {
	t.Helper()

	b, err := balancer.NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})

	srv := httptest.NewServer(newMux(pool,
		newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second),
		metrics.New(), notProxied(t)))
	t.Cleanup(srv.Close)
	return srv, pool
}

func getStatus(t *testing.T, url string) int {
	t.Helper()
	resp, err := http.Get(url)
	require.NoError(t, err)
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestLiveReturnsOK(t *testing.T) {
	srv, _ := newHealthzServer(t)
	assert.Equal(t, http.StatusOK, getStatus(t, srv.URL+"/live"))
}

func TestReadyReturnsOKWhenBackendAvailable(t *testing.T) {
	srv, _ := newHealthzServer(t)
	assert.Equal(t, http.StatusOK, getStatus(t, srv.URL+"/ready"))
}

func TestReadyReturns503WhenBackendUnhealthy(t *testing.T) {
	srv, pool := newHealthzServer(t)
	pool.Backends()[0].Healthy.Store(false)

	assert.Equal(t, http.StatusServiceUnavailable, getStatus(t, srv.URL+"/ready"))
}

func TestReadyReturns503InsideCooldown(t *testing.T) {
	b, err := balancer.NewBackend("http://127.0.0.1:9001",
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(time.Hour),
	)
	require.NoError(t, err)
	b.ReportFailure() // opens the circuit (threshold 1)

	pool := balancer.NewPool([]*balancer.Backend{b})
	srv := httptest.NewServer(newMux(pool,
		newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second),
		metrics.New(), notProxied(t)))
	t.Cleanup(srv.Close)

	assert.Equal(t, http.StatusServiceUnavailable, getStatus(t, srv.URL+"/ready"))
}

func TestHealthzWrongMethodNotAllowed(t *testing.T) {
	srv, _ := newHealthzServer(t)

	for _, path := range []string{"/live", "/ready"} {
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		require.NoError(t, err)

		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "POST %s", path)
		assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"), "POST %s", path)
	}
}

// TestReadyReturns200AfterCooldown is the other half of
// TestReadyReturns503InsideCooldown, at the HTTP level: past the cooldown the
// backend takes traffic again as the probe, so /ready must say so instead of
// contradicting the routing decision. Review finding 4; its passivity half is
// pinned white-box in internal/balancer, as TestPoolReadyIsPassive.
func TestReadyReturns200AfterCooldown(t *testing.T) {
	b, err := balancer.NewBackend("http://127.0.0.1:9001",
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(20*time.Millisecond),
	)
	require.NoError(t, err)
	b.ReportFailure() // open the circuit

	pool := balancer.NewPool([]*balancer.Backend{b})
	srv := httptest.NewServer(newMux(pool,
		newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second),
		metrics.New(), notProxied(t)))
	t.Cleanup(srv.Close)

	assert.Equal(t, http.StatusServiceUnavailable, getStatus(t, srv.URL+"/ready"),
		"inside the cooldown /ready reports 503")

	time.Sleep(40 * time.Millisecond)
	assert.Equal(t, http.StatusOK, getStatus(t, srv.URL+"/ready"),
		"past the cooldown the backend is the probe, so /ready reports 200")
}