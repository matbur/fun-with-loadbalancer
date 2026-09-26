package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
)

// hangingBackends builds n backends whose /healthz never answers, so every probe
// costs the full timeout — the case that makes a sequential cycle expensive.
func hangingBackends(t *testing.T, n int) []*balancer.Backend {
	t.Helper()

	backends := make([]*balancer.Backend, 0, n)
	for range n {
		hang := make(chan struct{})
		s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-hang
		}))
		t.Cleanup(func() { close(hang); s.Close() })

		b, err := balancer.NewBackend(s.URL)
		require.NoError(t, err)
		backends = append(backends, b)
	}
	return backends
}

// TestCheckerCycleCostDoesNotScaleWithPoolSize is the regression test for review
// finding 5. The checker probed backends one at a time, so a cycle cost up to
// len(backends) * timeout — measured at exactly 1.6s for 8 hanging backends with
// a 200ms timeout. Since a time.Ticker drops ticks while a cycle is in progress,
// a cycle that outlasts -health-check-every makes the effective interval the
// cycle duration, and the last backend in the list is not looked at until every
// earlier one has timed out. Probing in parallel brings the cycle back to about
// one timeout.
func TestCheckerCycleCostDoesNotScaleWithPoolSize(t *testing.T) {
	const timeout = 200 * time.Millisecond

	backends := hangingBackends(t, 8)
	c := New(balancer.NewPool(backends), WithTimeout(timeout), WithInterval(time.Hour))

	start := time.Now()
	c.checkAll(context.Background())
	elapsed := time.Since(start)

	// Generous ceiling: one timeout plus slack for a loaded CI machine, well
	// under the 8 * timeout a sequential cycle needs.
	assert.Less(t, elapsed, 4*timeout,
		"a cycle must cost about one timeout, not len(backends) * timeout")
}

// TestCheckerAppliesResultsPerBackend guards the fan-out's bookkeeping: results
// are collected into a shared slice and applied after the probes finish, so a
// backend's result has to land on that backend. A backend that answers and a
// neighbour that hangs must not trade verdicts.
func TestCheckerAppliesResultsPerBackend(t *testing.T) {
	alive := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(alive.Close)

	good, err := balancer.NewBackend(alive.URL)
	require.NoError(t, err)

	// One checker and one pool for the whole run: the failure counter lives in
	// the checker, so a fresh checker each round would never reach the threshold.
	backends := append([]*balancer.Backend{good}, hangingBackends(t, 4)...)
	c := New(balancer.NewPool(backends),
		WithTimeout(50*time.Millisecond),
		WithInterval(time.Hour))

	for round := 1; round <= failureThreshold; round++ {
		c.checkAll(context.Background())

		assert.True(t, good.Healthy.Load(), "the answering backend must stay healthy")
		for i, b := range backends[1:] {
			if round < failureThreshold {
				assert.True(t, b.Healthy.Load(),
					"hanging backend %d must not be marked unhealthy before the threshold", i)
				continue
			}
			assert.False(t, b.Healthy.Load(),
				"hanging backend %d must be unhealthy after %d consecutive failures", i, failureThreshold)
		}
	}
}

// TestCheckerProbesConcurrentlyUpToParallelism pins the bound: probes run
// together, but never more than the configured number at a time, so a large pool
// cannot open a connection per backend on every interval.
func TestCheckerProbesConcurrentlyUpToParallelism(t *testing.T) {
	peak := probePeakConcurrency(t, 8, 3)
	assert.Equal(t, int32(3), peak, "probes must run in parallel, bounded by parallelism")
}

func TestCheckerParallelismZeroProbesEverythingAtOnce(t *testing.T) {
	peak := probePeakConcurrency(t, 8, 0)
	assert.Equal(t, int32(8), peak, "parallelism 0 means no cap")
}

// TestCheckerParallelismAbovePoolSizeIsHarmless covers a bound larger than the
// pool: the cap must not become a floor, and the cycle must still finish.
func TestCheckerParallelismAbovePoolSizeIsHarmless(t *testing.T) {
	backends := make([]*balancer.Backend, 0, 3)
	for _, raw := range []string{"http://127.0.0.1:1", "http://127.0.0.1:2", "http://127.0.0.1:3"} {
		b, err := balancer.NewBackend(raw)
		require.NoError(t, err)
		backends = append(backends, b)
	}

	c := New(balancer.NewPool(backends), WithParallelism(64), WithInterval(time.Hour))

	done := make(chan struct{})
	go func() {
		c.checkAll(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("checkAll did not finish with parallelism above the pool size")
	}
}

// probePeakConcurrency runs one cycle against n backends that all block until
// released, and reports the highest number of probes that were ever in flight.
func probePeakConcurrency(t *testing.T, n, parallelism int) int32 {
	t.Helper()

	var inFlight, peak atomic.Int32
	release := make(chan struct{})
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		<-release
		inFlight.Add(-1)
	}))
	t.Cleanup(s.Close)

	backends := make([]*balancer.Backend, 0, n)
	for range n {
		b, err := balancer.NewBackend(s.URL)
		require.NoError(t, err)
		backends = append(backends, b)
	}

	c := New(balancer.NewPool(backends),
		WithParallelism(parallelism),
		WithTimeout(time.Second),
		WithInterval(time.Hour))

	done := make(chan struct{})
	go func() {
		c.checkAll(context.Background())
		close(done)
	}()

	// Wait for the wave to build up, then let it drain.
	for inFlight.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("checkAll did not finish")
	}
	return peak.Load()
}
