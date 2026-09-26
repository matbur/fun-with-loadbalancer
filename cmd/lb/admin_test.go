package main

import (
	"encoding/json"
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

func newTestAdmin(t *testing.T) (*adminAPI, *httptest.Server, *balancer.Pool) {
	t.Helper()

	b, err := balancer.NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})
	admin := newAdminAPI(pool, http.DefaultTransport, 3, 5*time.Second)

	// The production mux, so the routing under test is the routing that ships:
	// a method mismatch on a reserved path must be rejected here, not handed to
	// the catch-all proxy.
	srv := httptest.NewServer(newMux(pool, admin, metrics.New(), notProxied(t)))
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