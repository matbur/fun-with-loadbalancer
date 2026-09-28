package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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
	m := metrics.New()

	srv := httptest.NewServer(newMux(pool,
		newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second),
		m, notProxied(t)))
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
	m := metrics.New()
	srv := httptest.NewServer(newMux(pool,
		newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second),
		m, notProxied(t)))
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
	m := metrics.New()
	srv := httptest.NewServer(newMux(pool,
		newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second),
		m, notProxied(t)))
	t.Cleanup(srv.Close)

	assert.Equal(t, http.StatusServiceUnavailable, getStatus(t, srv.URL+"/ready"),
		"inside the cooldown /ready reports 503")

	time.Sleep(40 * time.Millisecond)
	assert.Equal(t, http.StatusOK, getStatus(t, srv.URL+"/ready"),
		"past the cooldown the backend is the probe, so /ready reports 200")
}

// notProxied stands in for the catch-all proxy and fails the test if it is ever
// reached, so a reserved path cannot quietly be served by a backend.
func notProxied(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request %s %s was proxied to a backend; reserved paths must not fall through to the catch-all",
			r.Method, r.URL.Path)
		http.Error(w, "should not be proxied", http.StatusTeapot)
	})
}

// TestReservedPathsRejectWrongMethod pins the routing rule: a request that
// differs from a reserved path only by method gets 405, not a proxied response.
// The proxy is registered as "/", which matches every path, so ServeMux's own
// 405 handling never applies to /backends, /live and /ready.
func TestReservedPathsRejectWrongMethod(t *testing.T) {
	b, err := balancer.NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})
	m := metrics.New()
	admin := newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second)

	srv := httptest.NewServer(newMux(pool, admin, m, notProxied(t)))
	defer srv.Close()

	// Only methods the path does not implement. DELETE /backends and POST
	// /backends are implemented (they reach admin.remove/admin.add), so they
	// are not candidates here.
	wrong := map[string][]string{
		"/backends":          {http.MethodPut, http.MethodPatch},
		"/live":              {http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		"/ready":             {http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		"/admin":             {http.MethodPost, http.MethodPut, http.MethodDelete},
		"/admin/":            {http.MethodPost, http.MethodPut, http.MethodDelete},
		"/admin/favicon.svg": {http.MethodPost, http.MethodPut, http.MethodDelete},
	}
	for path, methods := range wrong {
		for _, method := range methods {
			req, err := http.NewRequest(method, srv.URL+path, nil)
			require.NoError(t, err)

			resp, err := http.DefaultClient.Do(req)
			require.NoError(t, err)
			resp.Body.Close()

			assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, "%s %s", method, path)
			assert.NotEmpty(t, resp.Header.Get("Allow"), "%s %s must advertise Allow", method, path)
		}
	}
}

// TestReservedPathsServeGet confirms the fix did not cost the reserved paths
// their happy path.
func TestReservedPathsServeGet(t *testing.T) {
	b, err := balancer.NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})
	m := metrics.New()
	admin := newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second)

	srv := httptest.NewServer(newMux(pool, admin, m, notProxied(t)))
	defer srv.Close()

	for path, want := range map[string]int{
		"/live":              http.StatusOK,
		"/ready":             http.StatusOK,
		"/backends":          http.StatusOK,
		"/metrics":           http.StatusOK,
		"/admin":             http.StatusOK,
		"/admin/":            http.StatusOK,
		"/admin/favicon.svg": http.StatusOK,
	} {
		assert.Equal(t, want, getStatus(t, srv.URL+path), "GET %s", path)
	}
}

// TestProxyStillCatchAll is the other half of the contract: everything that is
// not a reserved path must still reach the proxy.
func TestProxyStillCatchAll(t *testing.T) {
	b, err := balancer.NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})
	m := metrics.New()
	admin := newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second)

	var proxied atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(newMux(pool, admin, m, handler))
	defer srv.Close()

	for _, path := range []string{"/", "/hello", "/api/v1/items", "/backends/extra", "/live/deep"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err)
		resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "GET %s", path)
	}
	assert.Equal(t, int32(5), proxied.Load(), "every non-reserved path must reach the proxy")
}

// TestMethodRouter covers the dispatcher directly, including the 405 body and
// the sorted Allow header.
func TestMethodRouter(t *testing.T) {
	router := methodRouter{
		http.MethodPost: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) },
		http.MethodGet:  func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) },
	}

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", nil))
	assert.Equal(t, http.StatusCreated, rec.Code)

	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec = httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, "/", nil))
		assert.Equal(t, http.StatusMethodNotAllowed, rec.Code, method)
		assert.Equal(t, "GET, POST", rec.Header().Get("Allow"), method)
		assert.Contains(t, rec.Body.String(), "method not allowed")
	}
}
