package balancer

import (
	"fmt"
	"sync"
	"testing"
	"time"

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

func TestPoolReady(t *testing.T) {
	b := mustBackend(t, "http://127.0.0.1:1")
	pool := NewPool([]*Backend{b})
	assert.True(t, pool.Ready(), "a fresh backend is ready")

	b.Healthy.Store(false)
	assert.False(t, pool.Ready(), "an unhealthy backend must not make the pool ready")

	b.Healthy.Store(true)
	b.breaker.open()
	assert.False(t, pool.Ready(), "a backend behind an open circuit must not make the pool ready")
}

func TestPoolReadyEmpty(t *testing.T) {
	assert.False(t, NewPool(nil).Ready(), "an empty pool is never ready")
}

func TestPoolReadyMixed(t *testing.T) {
	down := mustBackend(t, "http://127.0.0.1:1")
	down.Healthy.Store(false)
	up := mustBackend(t, "http://127.0.0.1:2")

	pool := NewPool([]*Backend{down, up})
	assert.True(t, pool.Ready(), "one usable backend is enough")
}

func TestPoolReadyViaFailures(t *testing.T) {
	b, err := NewBackend("http://127.0.0.1:1",
		WithBreakerThreshold(1),
		WithBreakerCooldown(time.Hour),
	)
	require.NoError(t, err)

	pool := NewPool([]*Backend{b})
	assert.True(t, pool.Ready())

	// One transport failure (threshold 1) opens the circuit: the pool stops
	// being ready even though the backend is still marked healthy.
	b.ReportFailure()
	assert.False(t, pool.Ready(), "an open circuit must make the pool not ready")
}

// TestPoolReadyAfterCooldown is the regression test for review finding 4: Ready
// used a bare "is the circuit open" read, so a circuit past its cooldown
// reported 503 while Next already handed the backend out as the probe.
func TestPoolReadyAfterCooldown(t *testing.T) {
	b, err := NewBackend("http://127.0.0.1:1",
		WithBreakerThreshold(1),
		WithBreakerCooldown(20*time.Millisecond),
	)
	require.NoError(t, err)
	pool := NewPool([]*Backend{b})

	b.ReportFailure()
	assert.False(t, pool.Ready(), "inside the cooldown the circuit blocks traffic")

	time.Sleep(40 * time.Millisecond)
	assert.True(t, pool.Ready(), "past the cooldown the backend takes traffic as the probe")
	assert.Same(t, b, pool.Next(), "and Next hands out the backend /ready just vouched for")
}

// TestPoolReadyIsPassive pins the other half: agreeing with Next must not mean
// performing the transition. Orchestrators poll /ready every second, so it may
// not move the breaker.
func TestPoolReadyIsPassive(t *testing.T) {
	b, err := NewBackend("http://127.0.0.1:1",
		WithBreakerThreshold(1),
		WithBreakerCooldown(20*time.Millisecond),
	)
	require.NoError(t, err)
	pool := NewPool([]*Backend{b})

	b.ReportFailure()
	time.Sleep(40 * time.Millisecond)

	for range 3 {
		assert.True(t, pool.Ready())
	}
	assert.Equal(t, circuitOpen, circuitState(b.breaker.state.Load()),
		"probing readiness must not transition open → half-open")
	assert.Zero(t, b.breaker.probeAt.Load(), "nor consume the probe")
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

func TestPoolAddBackend(t *testing.T) {
	pool := NewPool(nil)
	b := mustBackend(t, "http://127.0.0.1:1")

	require.NoError(t, pool.AddBackend(b))
	assert.Equal(t, []*Backend{b}, pool.Backends())
	assert.Same(t, b, pool.Next())

	// A duplicate add is rejected and must not change the list.
	require.ErrorIs(t, pool.AddBackend(b), ErrDuplicateBackend)
	assert.Len(t, pool.Backends(), 1)
}

func TestPoolAddNilBackend(t *testing.T) {
	pool := NewPool(nil)
	require.Error(t, pool.AddBackend(nil))
	assert.Empty(t, pool.Backends())
}

func TestPoolRemoveBackend(t *testing.T) {
	b := mustBackend(t, "http://127.0.0.1:1")
	pool := NewPool([]*Backend{b})

	require.NoError(t, pool.RemoveBackend(b))
	assert.Empty(t, pool.Backends())
	assert.Nil(t, pool.Next(), "a removed backend must not be selected")

	require.ErrorIs(t, pool.RemoveBackend(b), ErrBackendNotFound)
}

func TestPoolRemoveBackendResetsWeightedState(t *testing.T) {
	b := mustBackend(t, "http://127.0.0.1:1")
	pool := NewPool([]*Backend{b}, WithAlgorithm(AlgorithmWeightedRoundRobin))

	// Simulate a backend mid smooth-WRR cycle (e.g. accumulated weight after
	// several selections).
	b.currentWeight = 5

	require.NoError(t, pool.RemoveBackend(b))
	assert.Zero(t, b.currentWeight, "removal must reset the smooth-WRR accumulator")

	// Re-adding starts a fresh backend, not one mid-cycle.
	require.NoError(t, pool.AddBackend(b))
	assert.Equal(t, []*Backend{b}, pool.Backends())
}

func TestPoolConcurrentAddRemove(t *testing.T) {
	pool := NewPool(nil)

	const workers = 8
	const perWorker = 25

	// Pre-create backends so the goroutines never touch t through
	// require/assert helpers.
	all := make([][]*Backend, workers)
	for w := range workers {
		for i := range perWorker {
			b, err := NewBackend(fmt.Sprintf("http://127.0.0.1:%d", 2000+w*100+i))
			require.NoError(t, err)
			all[w] = append(all[w], b)
		}
	}

	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for _, b := range all[w] {
				if err := pool.AddBackend(b); err != nil {
					t.Errorf("AddBackend: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	assert.Len(t, pool.Backends(), workers*perWorker, "no concurrent add may be lost")

	for w := range workers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for _, b := range all[w] {
				if err := pool.RemoveBackend(b); err != nil {
					t.Errorf("RemoveBackend: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()

	assert.Empty(t, pool.Backends())
}
