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
	assert.ErrorContains(t, err, "breaker-cooldown")
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
func TestParseLimitDefaults(t *testing.T) {
	cfg, err := Parse([]string{"-backends", "http://127.0.0.1:1"})
	require.NoError(t, err)
	assert.Equal(t, int64(1<<20), cfg.MaxBodyBytes)
	assert.Equal(t, 0, cfg.MaxConcurrent)
}

func TestParseLimitFlags(t *testing.T) {
	cfg, err := Parse([]string{
		"-backends", "http://127.0.0.1:1",
		"-max-body-bytes", "4096",
		"-max-concurrent", "10",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(4096), cfg.MaxBodyBytes)
	assert.Equal(t, 10, cfg.MaxConcurrent)
}

func TestParseNegativeLimitsRejected(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-max-body-bytes", "-1"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "max-body-bytes")

	_, err = Parse([]string{"-backends", "http://127.0.0.1:1", "-max-concurrent", "-5"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "max-concurrent")
}

func TestParseConfigFileLimitFields(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", "max_body_bytes: 2048\nmax_concurrent: 4\nbackends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, int64(2048), cfg.MaxBodyBytes)
	assert.Equal(t, 4, cfg.MaxConcurrent)
}

func TestParseFlagsOverrideConfigLimits(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", "max_body_bytes: 2048\nbackends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path, "-max-body-bytes", "1024"})
	require.NoError(t, err)
	assert.Equal(t, int64(1024), cfg.MaxBodyBytes)
}

// TestParseServerTimeoutDefaults pins why the defaults are 0 and not some
// small number: a whole-request deadline shorter than it takes to upload
// -max-body-bytes pre-empts the documented 413 and the error is charged to the
// backend as a transport failure. Review finding 7.
func TestParseServerTimeoutDefaults(t *testing.T) {
	cfg, err := Parse([]string{"-backends", "http://127.0.0.1:1"})
	require.NoError(t, err)
	assert.Zero(t, cfg.ReadTimeout, "no deadline by default; -max-body-bytes bounds the body")
	assert.Zero(t, cfg.WriteTimeout, "no deadline by default; long responses must not be truncated")
}

func TestParseServerTimeoutFlags(t *testing.T) {
	cfg, err := Parse([]string{
		"-backends", "http://127.0.0.1:1",
		"-read-timeout", "45s",
		"-write-timeout", "2m",
	})
	require.NoError(t, err)
	assert.Equal(t, 45*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 2*time.Minute, cfg.WriteTimeout)
}

func TestParseNegativeServerTimeoutsRejected(t *testing.T) {
	_, err := Parse([]string{"-backends", "http://127.0.0.1:1", "-read-timeout", "-1s"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "read-timeout")

	_, err = Parse([]string{"-backends", "http://127.0.0.1:1", "-write-timeout", "-1s"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "write-timeout")
}

func TestParseConfigFileServerTimeouts(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", "read_timeout: 20s\nwrite_timeout: 90s\nbackends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, 20*time.Second, cfg.ReadTimeout)
	assert.Equal(t, 90*time.Second, cfg.WriteTimeout)
}

// TestParseHealthCheckParallelism pins the default and why it is bounded: a
// cycle must stay under -health-check-every for the configured interval to be
// the real one, and with the default 10s/2s a bound of 16 covers 80 backends.
// Review finding 5.
func TestParseHealthCheckParallelism(t *testing.T) {
	cfg, err := Parse([]string{"-backends", "http://127.0.0.1:1"})
	require.NoError(t, err)
	assert.Equal(t, 16, cfg.HealthCheckParallelism)

	cfg, err = Parse([]string{
		"-backends", "http://127.0.0.1:1",
		"-health-check-parallelism", "4",
	})
	require.NoError(t, err)
	assert.Equal(t, 4, cfg.HealthCheckParallelism)

	cfg, err = Parse([]string{
		"-backends", "http://127.0.0.1:1",
		"-health-check-parallelism", "0",
	})
	require.NoError(t, err)
	assert.Equal(t, 0, cfg.HealthCheckParallelism, "0 = no cap")

	_, err = Parse([]string{"-backends", "http://127.0.0.1:1", "-health-check-parallelism", "-1"})
	require.Error(t, err)
	assert.ErrorContains(t, err, "health-check-parallelism")
}

func TestParseConfigFileHealthCheckParallelism(t *testing.T) {
	path := writeConfigFile(t, "lb.yaml", "health_check_parallelism: 32\nbackends: [http://localhost:9001]\n")
	cfg, err := Parse([]string{"-config", path})
	require.NoError(t, err)
	assert.Equal(t, 32, cfg.HealthCheckParallelism)
}

// TestParseFlagsOverrideConfigFileAllFields asserts the documented precedence
// for every configurable field: an explicitly set flag wins over the config
// file value. Each subtest sets one field via a flag while the same field is
// also present in the file — this is the combination that used to lose the
// flag for breaker_threshold / breaker_cooldown / health_check_every /
// health_check_timeout.
func TestParseFlagsOverrideConfigFileAllFields(t *testing.T) {
	tests := []struct {
		name      string
		fileYAML  string
		flagArgs  []string
		get       func(Config) any
		flagValue any
	}{
		{
			name:      "listen",
			fileYAML:  "listen: \":9090\"\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-listen", ":7777"},
			get:       func(c Config) any { return c.ListenAddr },
			flagValue: ":7777",
		},
		{
			name:      "backends",
			fileYAML:  "backends: [http://localhost:9001, http://localhost:9002]\n",
			flagArgs:  []string{"-backends", "http://localhost:9003"},
			get:       func(c Config) any { return c.Backends },
			flagValue: []string{"http://localhost:9003"},
		},
		{
			name:      "health_check_every",
			fileYAML:  "health_check_every: 5s\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-health-check-every", "1s"},
			get:       func(c Config) any { return c.HealthCheckEvery },
			flagValue: time.Second,
		},
		{
			name:      "health_check_timeout",
			fileYAML:  "health_check_timeout: 2s\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-health-check-timeout", "500ms"},
			get:       func(c Config) any { return c.HealthCheckTimeout },
			flagValue: 500 * time.Millisecond,
		},
		{
			name:      "algorithm",
			fileYAML:  "algorithm: weighted-round-robin\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-algorithm", "least-connections"},
			get:       func(c Config) any { return c.Algorithm },
			flagValue: "least-connections",
		},
		{
			name:      "weights",
			fileYAML:  "weights: [2]\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-weights", "3"},
			get:       func(c Config) any { return c.Weights },
			flagValue: []int{3},
		},
		{
			name:      "breaker_threshold",
			fileYAML:  "breaker_threshold: 9\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-breaker-threshold", "1"},
			get:       func(c Config) any { return c.BreakerThreshold },
			flagValue: 1,
		},
		{
			name:      "breaker_cooldown",
			fileYAML:  "breaker_cooldown: 9s\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-breaker-cooldown", "1s"},
			get:       func(c Config) any { return c.BreakerCooldown },
			flagValue: time.Second,
		},
		{
			name:      "log_format",
			fileYAML:  "log_format: json\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-log-format", "text"},
			get:       func(c Config) any { return c.LogFormat },
			flagValue: "text",
		},
		{
			name:      "log_level",
			fileYAML:  "log_level: debug\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-log-level", "warn"},
			get:       func(c Config) any { return c.LogLevel },
			flagValue: "warn",
		},
		{
			name:      "max_body_bytes",
			fileYAML:  "max_body_bytes: 2048\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-max-body-bytes", "512"},
			get:       func(c Config) any { return c.MaxBodyBytes },
			flagValue: int64(512),
		},
		{
			name:      "max_concurrent",
			fileYAML:  "max_concurrent: 4\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-max-concurrent", "2"},
			get:       func(c Config) any { return c.MaxConcurrent },
			flagValue: 2,
		},
		{
			name:      "read_timeout",
			fileYAML:  "read_timeout: 9s\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-read-timeout", "1s"},
			get:       func(c Config) any { return c.ReadTimeout },
			flagValue: time.Second,
		},
		{
			name:      "write_timeout",
			fileYAML:  "write_timeout: 9s\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-write-timeout", "1s"},
			get:       func(c Config) any { return c.WriteTimeout },
			flagValue: time.Second,
		},
		{
			name:      "health_check_parallelism",
			fileYAML:  "health_check_parallelism: 9\nbackends: [http://localhost:9001]\n",
			flagArgs:  []string{"-health-check-parallelism", "2"},
			get:       func(c Config) any { return c.HealthCheckParallelism },
			flagValue: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfigFile(t, "lb.yaml", tt.fileYAML)
			args := append([]string{"-config", path}, tt.flagArgs...)
			cfg, err := Parse(args)
			require.NoError(t, err)
			assert.Equal(t, tt.flagValue, tt.get(cfg), "explicit flag must win over the file value")
		})
	}
}
