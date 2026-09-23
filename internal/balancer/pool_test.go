package balancer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func mustBackend(t *testing.T, rawURL string) *Backend {
	t.Helper()
	b, err := NewBackend(rawURL)
	require.NoError(t, err)
	return b
}

func TestPoolNextEmpty(t *testing.T) {
	pool := NewPool(nil)
	assert.Nil(t, pool.Next())
}

func TestPoolNextAllUnhealthy(t *testing.T) {
	b := mustBackend(t, "http://127.0.0.1:1")
	b.Healthy.Store(false)

	pool := NewPool([]*Backend{b})
	assert.Nil(t, pool.Next())
}

func TestPoolNextRoundRobinDistributesEvenly(t *testing.T) {
	backends := []*Backend{
		mustBackend(t, "http://127.0.0.1:1"),
		mustBackend(t, "http://127.0.0.1:2"),
	}
	pool := NewPool(backends)

	counts := map[*Backend]int{}
	const total = 100
	for i := 0; i < total; i++ {
		counts[pool.Next()]++
	}

	assert.Equal(t, total/2, counts[backends[0]])
	assert.Equal(t, total/2, counts[backends[1]])
}

func TestPoolNextSkipsUnhealthy(t *testing.T) {
	good := mustBackend(t, "http://127.0.0.1:2")
	bad := mustBackend(t, "http://127.0.0.1:1")
	bad.Healthy.Store(false)

	pool := NewPool([]*Backend{bad, good})
	for range 10 {
		assert.Same(t, good, pool.Next())
	}
}

func TestPoolNextIteratesPastUnhealthy(t *testing.T) {
	b0 := mustBackend(t, "http://127.0.0.1:1")
	b0.Healthy.Store(false)
	b1 := mustBackend(t, "http://127.0.0.1:2")
	b2 := mustBackend(t, "http://127.0.0.1:3")

	pool := NewPool([]*Backend{b0, b1, b2})

	counts := map[*Backend]int{}
	for i := 0; i < 100; i++ {
		next := pool.Next()
		if next == nil {
			t.Fatalf("Next() returned nil despite a healthy backend")
		}
		if next == b0 {
			t.Fatalf("unhealthy backend was selected")
		}
		counts[next]++
	}

	assert.Equal(t, 0, counts[b0])
	assert.Equal(t, 100, counts[b1]+counts[b2])
}
