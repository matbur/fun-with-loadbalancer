package balancer

import (
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