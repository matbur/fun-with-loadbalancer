package balancer

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newBreakerBackend(t *testing.T, threshold int, cooldown time.Duration) *Backend {
	t.Helper()
	b, err := NewBackend(
		"http://127.0.0.1:1",
		WithBreakerThreshold(threshold),
		WithBreakerCooldown(cooldown),
	)
	require.NoError(t, err)
	return b
}

func TestCircuitBreakerStartsClosed(t *testing.T) {
	b := newBreakerBackend(t, 3, time.Second)
	assert.True(t, b.Available())
}

func TestCircuitBreakerOpensAfterThresholdFailures(t *testing.T) {
	b := newBreakerBackend(t, 3, time.Second)

	for range 2 {
		b.ReportFailure()
		assert.True(t, b.Available(), "below the threshold the backend stays available")
	}
	b.ReportFailure()
	assert.False(t, b.Available(), "third consecutive failure must open the circuit")

	// A success while open must NOT close the circuit: no probes are allowed
	// until the cooldown elapses.
	b.ReportSuccess()
	assert.False(t, b.Available())
}

func TestCircuitBreakerSuccessResetsFailures(t *testing.T) {
	b := newBreakerBackend(t, 4, time.Second)

	b.ReportFailure()
	b.ReportFailure()
	b.ReportSuccess()

	// The success reset the counter; two more failures must not open it.
	b.ReportFailure()
	b.ReportFailure()
	assert.True(t, b.Available(), "success must reset the consecutive-failure counter")
}

func TestCircuitBreakerStayOpenUntilCooldown(t *testing.T) {
	b := newBreakerBackend(t, 3, 50*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	require.False(t, b.Available())

	time.Sleep(20 * time.Millisecond)
	assert.False(t, b.Available(), "still within the cooldown window")
}

func TestCircuitBreakerHalfOpenProbeSuccessCloses(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	require.False(t, b.Available())

	time.Sleep(40 * time.Millisecond)
	require.True(t, b.Available(), "after the cooldown a probe is allowed (half-open)")

	b.ReportSuccess()
	assert.True(t, b.Available(), "a successful probe must close the circuit")
}

func TestCircuitBreakerHalfOpenProbeFailureReopens(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	require.False(t, b.Available())

	time.Sleep(40 * time.Millisecond)
	require.True(t, b.Available())

	b.ReportFailure()
	assert.False(t, b.Available(), "a failed probe must reopen the circuit")

	// The reopened circuit needs a fresh cooldown.
	assert.False(t, b.Available())
}

// TestCircuitBreakerHalfOpenAdmitsSingleProbe is the regression test for
// single admission: before it existed every caller observing halfOpen was let
// through.
func TestCircuitBreakerHalfOpenAdmitsSingleProbe(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	require.False(t, b.Available())
	time.Sleep(40 * time.Millisecond)

	admitted := 0
	for range 100 {
		if b.Available() {
			admitted++
		}
	}
	assert.Equal(t, 1, admitted, "half-open must admit a single probe, not every concurrent request")
}

// TestCircuitBreakerHalfOpenAdmitsSingleProbeUnderConcurrency is the same
// property under real contention, which the sequential check cannot reach.
func TestCircuitBreakerHalfOpenAdmitsSingleProbeUnderConcurrency(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	require.False(t, b.Available())
	time.Sleep(40 * time.Millisecond)

	const goroutines = 64
	var admitted atomic.Int32
	var start, done sync.WaitGroup
	start.Add(1)
	done.Add(goroutines)
	for range goroutines {
		go func() {
			defer done.Done()
			start.Wait() // release them all at once
			if b.Available() {
				admitted.Add(1)
			}
		}()
	}
	start.Done()
	done.Wait()

	assert.Equal(t, int32(1), admitted.Load(), "exactly one goroutine may be the probe")
}

func TestCircuitBreakerHalfOpenProbeEndsAdmission(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	time.Sleep(40 * time.Millisecond)

	require.True(t, b.Available(), "first probe is admitted")
	assert.False(t, b.Available(), "admission is taken until the probe reports")

	b.ReportSuccess()
	assert.Equal(t, circuitClosed, circuitState(b.breaker.state.Load()))
	assert.Zero(t, b.breaker.probeAt.Load(), "a successful probe must give up its admission")
}

// TestCircuitBreakerHalfOpenReclaimsAbandonedProbe guards the one case that
// never reports, where nothing else would move the breaker.
func TestCircuitBreakerHalfOpenReclaimsAbandonedProbe(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	time.Sleep(40 * time.Millisecond)

	require.True(t, b.Available(), "first probe is admitted and never reports")
	assert.False(t, b.Available(), "a probe still in progress keeps its admission")

	// Simulate the probe dying: it was admitted longer ago than probeTimeout, so
	// the next caller must be allowed to take over.
	b.breaker.probeAt.Store(time.Now().Add(-probeTimeout - time.Second).UnixNano())
	assert.True(t, b.Available(), "a probe that never reports must be taken over from")
	assert.False(t, b.Available(), "taking over admits exactly one replacement")
}

// TestPoolHalfOpenBackendReceivesOneRequest is the pool-level view: the
// round-robin scan returns the first candidate it admits, probe included.
func TestPoolHalfOpenBackendReceivesOneRequest(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	time.Sleep(40 * time.Millisecond)

	pool := NewPool([]*Backend{b})
	assert.Same(t, b, pool.Next(), "the half-open backend is handed the probe")
	assert.Nil(t, pool.Next(), "no second request may be routed to the probe")

	b.ReportSuccess()
	assert.Same(t, b, pool.Next(), "after the probe succeeds the backend is back in rotation")
}

// TestCircuitBreakerReportHealthyRecoversOpenCircuit covers the out-of-band
// path, the only way back for an idle load balancer.
func TestCircuitBreakerReportHealthyRecoversOpenCircuit(t *testing.T) {
	b := newBreakerBackend(t, 3, time.Hour) // cooldown far in the future
	for range 3 {
		b.ReportFailure()
	}
	require.False(t, b.Available())
	require.Equal(t, circuitOpen, circuitState(b.breaker.state.Load()),
		"the circuit must be open, not half-open")

	b.ReportHealthy()
	assert.Equal(t, circuitClosed, circuitState(b.breaker.state.Load()))
	assert.True(t, b.Available(), "a recovered backend must take traffic again")
	assert.Zero(t, b.breaker.probeAt.Load(), "recovery must end any probe in progress")
}

// TestCircuitBreakerReportHealthyLeavesClosedCircuitAlone guards the other side:
// recovery must not clear a closed circuit's failure counter, or a backend
// failing one request in five would never reach the threshold.
func TestCircuitBreakerReportHealthyLeavesClosedCircuitAlone(t *testing.T) {
	b := newBreakerBackend(t, 3, time.Hour)

	b.ReportFailure()
	b.ReportFailure()
	b.ReportHealthy() // a health check lands between the failures
	assert.Equal(t, circuitClosed, circuitState(b.breaker.state.Load()))

	b.ReportFailure() // third consecutive failure must still open the circuit
	assert.Equal(t, circuitOpen, circuitState(b.breaker.state.Load()),
		"health checks must not reset the failure counter")
}

// TestCircuitBreakerReportHealthyRecoversHalfOpen: the check resolves the
// circuit before the admitted probe reports.
func TestCircuitBreakerReportHealthyRecoversHalfOpen(t *testing.T) {
	b := newBreakerBackend(t, 3, 20*time.Millisecond)
	for range 3 {
		b.ReportFailure()
	}
	time.Sleep(40 * time.Millisecond)
	require.True(t, b.Available(), "the probe is admitted and then stalls")

	b.ReportHealthy()
	assert.Equal(t, circuitClosed, circuitState(b.breaker.state.Load()))
	assert.True(t, b.Available(), "the backend is back in normal rotation")

	// The stalled probe's late report must not undo the recovery.
	b.ReportFailure()
	assert.Equal(t, circuitClosed, circuitState(b.breaker.state.Load()),
		"a late probe verdict must not re-open a circuit the health check closed")
}

func TestPoolSkipsBackendWithOpenBreaker(t *testing.T) {
	good := mustBackend(t, "http://127.0.0.1:2")
	broken := mustBackend(t, "http://127.0.0.1:1")
	for range 3 {
		broken.ReportFailure()
	}
	require.False(t, broken.Available())

	pool := NewPool([]*Backend{broken, good}, WithAlgorithm(AlgorithmLeastConnections))
	for range 10 {
		assert.Same(t, good, pool.Next(), "open circuit must exclude the backend from selection")
	}
}