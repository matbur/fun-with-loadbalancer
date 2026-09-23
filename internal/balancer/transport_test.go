package balancer

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewTransportSettings(t *testing.T) {
	tr := NewTransport()

	assert.Equal(t, 100, tr.MaxIdleConns)
	assert.Equal(t, 20, tr.MaxIdleConnsPerHost)
	assert.Equal(t, 100, tr.MaxConnsPerHost)
	assert.Equal(t, 90*time.Second, tr.IdleConnTimeout)
	assert.Equal(t, 2*time.Second, tr.TLSHandshakeTimeout)
	assert.Equal(t, 5*time.Second, tr.ResponseHeaderTimeout)
	assert.Equal(t, 1*time.Second, tr.ExpectContinueTimeout)
	assert.NotNil(t, tr.Proxy)
	assert.NotNil(t, tr.DialContext)
}

func TestNewBackendWithTransport(t *testing.T) {
	tr := NewTransport()

	b, err := NewBackend("http://127.0.0.1:9001", WithTransport(tr))
	require.NoError(t, err)

	assert.Same(t, tr, b.Proxy.Transport)
}