package balancer

import (
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