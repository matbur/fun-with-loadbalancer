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
	admin := newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second)

	srv := httptest.NewServer(newMux(pool, admin, metrics.New(), notProxied(t)))
	defer srv.Close()

	// Only methods the path does not implement. DELETE /backends and POST
	// /backends are implemented (they reach admin.remove/admin.add), so they
	// are not candidates here.
	wrong := map[string][]string{
		"/backends": {http.MethodPut, http.MethodPatch},
		"/live":     {http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
		"/ready":    {http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete},
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
	admin := newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second)

	srv := httptest.NewServer(newMux(pool, admin, metrics.New(), notProxied(t)))
	defer srv.Close()

	for path, want := range map[string]int{
		"/live":     http.StatusOK,
		"/ready":    http.StatusOK,
		"/backends": http.StatusOK,
		"/metrics":  http.StatusOK,
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
	admin := newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second)

	var proxied atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	})

	srv := httptest.NewServer(newMux(pool, admin, metrics.New(), handler))
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
