package main

import (
	"encoding/json"
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
	"fun-with-loadbalancer/internal/proxy"
)

func newTestAdmin(t *testing.T) (*adminAPI, *httptest.Server, *balancer.Pool) {
	t.Helper()

	b, err := balancer.NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})
	m := metrics.New()
	admin := newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second)

	// The production mux, so the routing under test is the routing that ships:
	// a method mismatch on a reserved path must be rejected here, not handed to
	// the catch-all proxy.
	srv := httptest.NewServer(newMux(pool, admin, m, notProxied(t)))
	t.Cleanup(srv.Close)
	return admin, srv, pool
}

func getBackends(t *testing.T, srv *httptest.Server) []backendInfo {
	t.Helper()
	resp, err := http.Get(srv.URL + "/backends")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var infos []backendInfo
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&infos))
	return infos
}

// TestAdminUIIsServed: both paths serve the panel, and neither is proxied.
func TestAdminUIIsServed(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	for _, path := range []string{"/admin", "/admin/"} {
		resp, err := http.Get(srv.URL + path)
		require.NoError(t, err, path)
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		require.NoError(t, err)

		assert.Equal(t, http.StatusOK, resp.StatusCode, path)
		assert.Equal(t, "text/html; charset=utf-8", resp.Header.Get("Content-Type"), path)
		assert.Contains(t, string(body), "<!DOCTYPE html>", path)
		// Self-contained: no CDN, no build step, so it works offline.
		assert.NotContains(t, string(body), "http://cdn", path)
		assert.NotContains(t, string(body), "https://cdn", path)
	}
}

func TestAdminUIRejectsWrongMethod(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL+"/admin", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		resp.Body.Close()

		assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode, method)
		assert.Equal(t, "GET, HEAD", resp.Header.Get("Allow"), method)
	}
}

func TestAdminListsBackends(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	infos := getBackends(t, srv)
	require.Len(t, infos, 1)
	assert.Equal(t, "http://127.0.0.1:9001", infos[0].URL)
	assert.Equal(t, 1, infos[0].Weight)
	assert.True(t, infos[0].Healthy)
	assert.Zero(t, infos[0].Active)
}

func TestAdminAddsBackend(t *testing.T) {
	_, srv, pool := newTestAdmin(t)

	resp, err := http.Post(srv.URL+"/backends", "application/json",
		strings.NewReader(`{"url":"http://127.0.0.1:9002","weight":3}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	infos := getBackends(t, srv)
	require.Len(t, infos, 2)

	var added *backendInfo
	for _, info := range infos {
		if info.URL == "http://127.0.0.1:9002" {
			added = &info
		}
	}
	require.NotNil(t, added)
	assert.Equal(t, 3, added.Weight)

	// The added backend is really available to the pool, not just listed.
	require.Len(t, pool.Backends(), 2)
}

func TestAdminAddDuplicateConflict(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	resp, err := http.Post(srv.URL+"/backends", "application/json",
		strings.NewReader(`{"url":"http://127.0.0.1:9001"}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestAdminAddInvalidBody(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	resp, err := http.Post(srv.URL+"/backends", "application/json", strings.NewReader("not-json"))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminAddInvalidURL(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	resp, err := http.Post(srv.URL+"/backends", "application/json",
		strings.NewReader(`{"url":"not-a-url"}`))
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestAdminRemovesBackend(t *testing.T) {
	_, srv, pool := newTestAdmin(t)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/backends",
		strings.NewReader(`{"url":"http://127.0.0.1:9001"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	require.Empty(t, pool.Backends())
	require.Empty(t, getBackends(t, srv))
}

// A real proxy, so a request can create the series DELETE is meant to drop.
func newAdminProxyServer(t *testing.T) (*httptest.Server, *balancer.Pool) {
	t.Helper()

	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(be.Close)

	b, err := balancer.NewBackend(be.URL)
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})

	m := metrics.New()
	proxied := m.Wrap(proxy.NewServer(pool, proxy.WithMetrics(m)))
	srv := httptest.NewServer(newMux(pool, newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second), m, proxied))
	t.Cleanup(srv.Close)

	return srv, pool
}

// Reads the exposition text, so the assertion is on what gets scraped.
func seriesFor(t *testing.T, srv *httptest.Server, metric, value string) []string {
	t.Helper()

	resp, err := http.Get(srv.URL + "/metrics")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	var out []string
	for line := range strings.SplitSeq(string(body), "\n") {
		if strings.HasPrefix(line, metric+"{") && strings.Contains(line, `"`+value+`"`) {
			out = append(out, line)
		}
	}
	return out
}

// End-to-end: a removed backend must stop appearing in /metrics.
func TestAdminRemoveDropsMetricSeries(t *testing.T) {
	srv, pool := newAdminProxyServer(t)
	url, host := pool.Backends()[0].URL.String(), pool.Backends()[0].URL.Host

	resp, err := http.Get(srv.URL + "/anything")
	require.NoError(t, err)
	resp.Body.Close()
	require.NotEmpty(t, seriesFor(t, srv, "lb_backend_requests_total", host),
		"a proxied request must record a backend series")

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/backends",
		strings.NewReader(`{"url":"`+url+`"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)

	assert.Empty(t, seriesFor(t, srv, "lb_backend_requests_total", host),
		"a removed backend must not stay in /metrics")
	assert.Empty(t, seriesFor(t, srv, "lb_backend_errors_total", host))
}

func TestAdminRemoveMissingNotFound(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	req, err := http.NewRequest(http.MethodDelete, srv.URL+"/backends",
		strings.NewReader(`{"url":"http://127.0.0.1:9999"}`))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestAdminWrongMethodNotAllowed(t *testing.T) {
	_, srv, _ := newTestAdmin(t)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/backends", nil)
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	resp.Body.Close()
	assert.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	assert.Equal(t, "DELETE, GET, HEAD, POST", resp.Header.Get("Allow"))
}
