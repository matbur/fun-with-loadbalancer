package config

import (
	"os"
	"path/filepath"
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

func TestParseInvalidAlgorithm(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-algorithm", "magic"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown algorithm")
}

func writeConfigFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	return path
}

func TestParseConfigFileYAML(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", `
listen: ":9090"
backends:
  - http://localhost:9001
  - http://localhost:9002
health_check_every: 5s
health_check_timeout: 1s
algorithm: weighted-round-robin
weights: [1, 2]
breaker_threshold: 4
breaker_cooldown: 7s
`)
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.ListenAddr)
	assert.Equal(t, []string{"http://localhost:9001", "http://localhost:9002"}, cfg.Backends)
	assert.Equal(t, 5*time.Second, cfg.HealthCheckEvery)
	assert.Equal(t, 1*time.Second, cfg.HealthCheckTimeout)
	assert.Equal(t, "weighted-round-robin", cfg.Algorithm)
	assert.Equal(t, []int{1, 2}, cfg.Weights)
	assert.Equal(t, 4, cfg.BreakerThreshold)
	assert.Equal(t, 7*time.Second, cfg.BreakerCooldown)
}

func TestParseConfigFileJSON(t *testing.T) {
	path := writeConfigFile(t, "lb.json", `{
  "listen": ":9090",
  "backends": ["http://localhost:9001"],
  "algorithm": "least-connections",
  "weights": [2],
  "breaker_threshold": 2,
  "breaker_cooldown": "3s"
}`)
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.ListenAddr)
	assert.Equal(t, []string{"http://localhost:9001"}, cfg.Backends)
	assert.Equal(t, "least-connections", cfg.Algorithm)
	assert.Equal(t, []int{2}, cfg.Weights)
	assert.Equal(t, 2, cfg.BreakerThreshold)
	assert.Equal(t, 3*time.Second, cfg.BreakerCooldown)
	// Not set in the file -> default from the flag.
	assert.Equal(t, 10*time.Second, cfg.HealthCheckEvery)
	assert.Equal(t, 2*time.Second, cfg.HealthCheckTimeout)
}

func TestParseConfigFileMinimal(t *testing.T) {
	path := writeConfigFile(t, "minimal.yaml", "backends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, ":8080", cfg.ListenAddr)
	assert.Equal(t, "round-robin", cfg.Algorithm)
	assert.Equal(t, 3, cfg.BreakerThreshold)
	assert.Equal(t, 5*time.Second, cfg.BreakerCooldown)
}

func TestParseFlagsOverrideConfigFile(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", `
listen: ":9090"
backends: [http://localhost:9001, http://localhost:9002]
algorithm: least-connections
`)
	cfg, err := Parse([]string{
		"-config", path,
		"-backends", "http://localhost:9003",
		"-algorithm", "round-robin",
	})
	require.NoError(t, err)
	assert.Equal(t, ":9090", cfg.ListenAddr, "listen is not overridden by a flag, so it stays from the file")
	assert.Equal(t, []string{"http://localhost:9003"}, cfg.Backends, "explicit -backends wins over the file")
	assert.Equal(t, "round-robin", cfg.Algorithm)
}

func TestParseConfigFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nope.yaml")
	_, err := Parse([]string{"-config", path})
	require.Error(t, err)
}

func TestParseConfigFileBadExtension(t *testing.T) {
	path := writeConfigFile(t, "lb.toml", "whatever")
	_, err := Parse([]string{"-config", path})
	require.Error(t, err)
	assert.ErrorContains(t, err, "extension")
}

func TestParseConfigFileMalformed(t *testing.T) {
	path := writeConfigFile(t, "bad.yaml", "listen: [unclosed")
	_, err := Parse([]string{"-config", path})
	require.Error(t, err)
	assert.ErrorContains(t, err, "yaml:")
}

func TestParseConfigFileBadDuration(t *testing.T) {
	path := writeConfigFile(t, "bad.yaml", "breaker_cooldown: banana\n")
	_, err := Parse([]string{"-config", path})
	require.Error(t, err)
	assert.ErrorContains(t, err, "breaker_cooldown")
}

func TestParseConfigFileBadAlgorithm(t *testing.T) {
	path := writeConfigFile(t, "bad.yaml", "algorithm: magic\nbackends: [http://localhost:9001]\n")
	_, err := Parse([]string{"-config", path})
	require.Error(t, err)
	assert.ErrorContains(t, err, "unknown algorithm")
}

func TestParseLogDefaults(t *testing.T) {
	cfg, err := Parse([]string{"-backends", "http://127.0.0.1:1"})
	require.NoError(t, err)
	assert.Equal(t, "text", cfg.LogFormat)
	assert.Equal(t, "info", cfg.LogLevel)
}

func TestParseLogFlags(t *testing.T) {
	cfg, err := Parse([]string{
		"-backends", "http://127.0.0.1:1",
		"-log-format", "json",
		"-log-level", "debug",
	})
	require.NoError(t, err)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestParseInvalidLogFormat(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-log-format", "xml"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "log format")
}

func TestParseInvalidLogLevel(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-log-level", "verbose"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "log level")
}

func TestParseConfigFileLogFields(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", "log_format: json\nlog_level: debug\nbackends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, "json", cfg.LogFormat)
	assert.Equal(t, "debug", cfg.LogLevel)
}

func TestParseFlagsOverrideConfigLogFields(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", "log_format: json\nbackends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path, "-log-format", "text"})
	require.NoError(t, err)
	assert.Equal(t, "text", cfg.LogFormat)
}