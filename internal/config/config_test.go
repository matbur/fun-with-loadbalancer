package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseAlgorithm(t *testing.T) {
	for _, name := range []string{"round-robin", "weighted-round-robin", "least-connections"} {
		cfg, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-algorithm", name})
		require.NoError(t, err)
		assert.Equal(t, name, cfg.Algorithm)
	}
}

func TestParseDefaults(t *testing.T) {
	cfg, err := Parse([]string{"-backends", "http://127.0.0.1:1,http://127.0.0.1:2"})
	require.NoError(t, err)

	assert.Equal(t, "round-robin", cfg.Algorithm)
	assert.Empty(t, cfg.Weights, "no -weights flag means all weights are 1")
	assert.Equal(t, 3, cfg.BreakerThreshold)
	assert.Equal(t, 5*time.Second, cfg.BreakerCooldown)
}

func TestParseWeights(t *testing.T) {
	cfg, err := Parse([]string{
		"-backends", "http://127.0.0.1:1,http://127.0.0.1:2,http://127.0.0.1:3",
		"-weights", "1,2,3",
	})
	require.NoError(t, err)
	assert.Equal(t, []int{1, 2, 3}, cfg.Weights)
}

func TestParseWeightsCountMismatch(t *testing.T) {
	_, err := Parse([]string{
		"-backends", "http://127.0.0.1:1,http://127.0.0.1:2",
		"-weights", "1",
	})
	require.Error(t, err)
	assert.ErrorContains(t, err, "weights count")
}

func TestParseInvalidWeight(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-weights", "abc"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "invalid weight")
}

func TestParseBreakerValues(t *testing.T) {
	cfg, err := Parse([]string{
		"-backends", "http://127.0.0.1:1",
		"-breaker-threshold", "1",
		"-breaker-cooldown", "100ms",
	})
	require.NoError(t, err)
	assert.Equal(t, 1, cfg.BreakerThreshold)
	assert.Equal(t, 100*time.Millisecond, cfg.BreakerCooldown)
}

func TestParseBreakerThresholdTooLow(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-breaker-threshold", "0"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "breaker-threshold")
}

func TestParseBreakerCooldownNegative(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-breaker-cooldown", "-1s"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "breaker-cooldown")
}

func TestParseRequiresBackend(t *testing.T) {
	_, err := Parse(nil)
	require.Error(t, err)
	assert.ErrorContains(t, err, "at least one backend")
}