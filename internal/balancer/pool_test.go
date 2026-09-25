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
	for range total {
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
	for range 100 {
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

func mustWeighted(t *testing.T, rawURL string, weight int) *Backend {
	t.Helper()
	b, err := NewBackend(rawURL, WithWeight(weight))
	require.NoError(t, err)
	return b
}

func TestWeightedRoundRobinDistributesByWeight(t *testing.T) {
	backends := []*Backend{
		mustWeighted(t, "http://127.0.0.1:1", 1),
		mustWeighted(t, "http://127.0.0.1:2", 2),
		mustWeighted(t, "http://127.0.0.1:3", 3),
	}
	pool := NewPool(backends, WithAlgorithm(AlgorithmWeightedRoundRobin))

	const total = 600
	counts := map[*Backend]int{}
	for range total {
		counts[pool.Next()]++
	}

	assert.Equal(t, total/6, counts[backends[0]], "weight 1")
	assert.Equal(t, total/3, counts[backends[1]], "weight 2")
	assert.Equal(t, total/2, counts[backends[2]], "weight 3")
}

func TestWeightedRoundRobinSkipsUnhealthy(t *testing.T) {
	down := mustWeighted(t, "http://127.0.0.1:1", 100)
	down.Healthy.Store(false)
	slow := mustWeighted(t, "http://127.0.0.1:2", 1)
	fast := mustWeighted(t, "http://127.0.0.1:3", 1)

	pool := NewPool([]*Backend{down, slow, fast}, WithAlgorithm(AlgorithmWeightedRoundRobin))

	const total = 100
	counts := map[*Backend]int{}
	for range total {
		next := pool.Next()
		require.NotNil(t, next)
		counts[next]++
	}

	assert.Equal(t, 0, counts[down])
	// Down backend's weight is redistributed: healthy ones share 50/50.
	assert.Equal(t, total/2, counts[slow])
	assert.Equal(t, total/2, counts[fast])
}

func TestLeastConnectionsPicksLeastActive(t *testing.T) {
	busy := mustBackend(t, "http://127.0.0.1:1")
	busy.Acquire()
	busy.Acquire()
	busy.Acquire()
	idle := mustBackend(t, "http://127.0.0.1:2")

	pool := NewPool([]*Backend{busy, idle}, WithAlgorithm(AlgorithmLeastConnections))

	for range 10 {
		assert.Same(t, idle, pool.Next())
	}
}

func TestLeastConnectionsSkipsUnhealthy(t *testing.T) {
	down := mustBackend(t, "http://127.0.0.1:1")
	down.Healthy.Store(false)
	down.Acquire()
	other := mustBackend(t, "http://127.0.0.1:2")

	pool := NewPool([]*Backend{down, other}, WithAlgorithm(AlgorithmLeastConnections))

	assert.Same(t, other, pool.Next())
}
