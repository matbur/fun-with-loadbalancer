package balancer

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBackendInvalidURL(t *testing.T) {
	for _, rawURL := range []string{"://bad", "no-scheme-or-host"} {
		t.Run(rawURL, func(t *testing.T) {
			_, err := NewBackend(rawURL)
			require.Error(t, err)
		})
	}
}

func TestNewBackendParsesURL(t *testing.T) {
	b, err := NewBackend("http://127.0.0.1:9001")
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:9001", b.URL.Host)
	assert.NotNil(t, b.Proxy)
	assert.True(t, b.Healthy.Load())
}

// TestBackendWrites502OnConnectionError verifies that an unreachable backend
// produces a 502 through ErrorHandler when no upstreamErrorWriter is present.
func TestBackendWrites502OnConnectionError(t *testing.T) {
	// Reserve a port and close the listener so dialing it fails with
	// connection refused.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())

	b, err := NewBackend("http://" + addr)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	b.Proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadGateway, rec.Code)
}

// TestBackendPassesThroughSuccess verifies a healthy backend response passes
// through unchanged.
func TestBackendPassesThroughSuccess(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	b, err := NewBackend(backend.URL)
	require.NoError(t, err)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://frontend.example/", nil)
	b.Proxy.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code)
}