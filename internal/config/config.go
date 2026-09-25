package config

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	ListenAddr         string
	Backends           []string
	HealthCheckEvery   time.Duration
	HealthCheckTimeout time.Duration

	// Algorithm is one of round-robin, weighted-round-robin,
	// least-connections (validated here; cmd/lb maps it to balancer).
	Algorithm string
	// Weights aligned with Backends; empty means 1 for every backend.
	Weights []int

	BreakerThreshold int
	BreakerCooldown  time.Duration

	// LogFormat is one of text, json; LogLevel one of debug, info, warn, error.
	LogFormat string
	LogLevel  string

	// MaxBodyBytes limits the request body size in bytes (0 = unlimited);
	// MaxConcurrent caps the number of in-flight requests (0 = unlimited).
	MaxBodyBytes int64
	MaxConcurrent int
}

// fileConfig mirrors Config for YAML/JSON files. Pointers and nil slices
// distinguish "absent from the file" from explicit zero values, so file
// contents can be merged with flags and defaults. Durations are strings
// (e.g. "5s") parsed with time.ParseDuration.
type fileConfig struct {
	ListenAddr         *string  `json:"listen" yaml:"listen"`
	Backends           []string `json:"backends" yaml:"backends"`
	HealthCheckEvery   *string  `json:"health_check_every" yaml:"health_check_every"`
	HealthCheckTimeout *string  `json:"health_check_timeout" yaml:"health_check_timeout"`
	Algorithm          *string  `json:"algorithm" yaml:"algorithm"`
	Weights            []int    `json:"weights" yaml:"weights"`
	BreakerThreshold   *int     `json:"breaker_threshold" yaml:"breaker_threshold"`
	BreakerCooldown    *string  `json:"breaker_cooldown" yaml:"breaker_cooldown"`
	LogFormat          *string  `json:"log_format" yaml:"log_format"`
	LogLevel           *string  `json:"log_level" yaml:"log_level"`
	MaxBodyBytes       *int64   `json:"max_body_bytes" yaml:"max_body_bytes"`
	MaxConcurrent      *int     `json:"max_concurrent" yaml:"max_concurrent"`
}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("lb", flag.ContinueOnError)

	var cfgFile string
	var listen string
	var backendsStr string
	var healthCheckEvery time.Duration
	var healthCheckTimeout time.Duration
	var algorithm string
	var weightsStr string
	var breakerThreshold int
	var breakerCooldown time.Duration
	var logFormat string
	var logLevel string
	var maxBodyBytes int64
	var maxConcurrent int

	fs.StringVar(&cfgFile, "config", "", "path to a YAML/JSON config file; explicitly set flags override file values")
	fs.StringVar(&listen, "listen", ":8080", "address to listen on")
	fs.StringVar(&backendsStr, "backends", "", "comma-separated backend URLs")
	fs.DurationVar(&healthCheckEvery, "health-check-every", 10*time.Second, "health check interval")
	fs.DurationVar(&healthCheckTimeout, "health-check-timeout", 2*time.Second, "health check per-backend timeout")
	fs.StringVar(&algorithm, "algorithm", "round-robin", "selection algorithm: round-robin | weighted-round-robin | least-connections")
	fs.StringVar(&weightsStr, "weights", "", "comma-separated backend weights (must match -backends count)")
	fs.IntVar(&breakerThreshold, "breaker-threshold", 3, "consecutive transport failures before a backend's circuit opens")
	fs.DurationVar(&breakerCooldown, "breaker-cooldown", 5*time.Second, "how long an open circuit stays open")
	fs.StringVar(&logFormat, "log-format", "text", "log output format: text | json")
	fs.StringVar(&logLevel, "log-level", "info", "log level: debug | info | warn | error")
	fs.Int64Var(&maxBodyBytes, "max-body-bytes", 1<<20, "max request body size in bytes (0 = unlimited)")
	fs.IntVar(&maxConcurrent, "max-concurrent", 0, "max concurrent in-flight requests (0 = unlimited)")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	// Explicitly set flags override the config file; anything the file does
	// not define falls back to the flag defaults.
	set := make(map[string]bool)
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })

	backends := splitCSV(backendsStr)
	weights, err := parseWeights(weightsStr)
	if err != nil {
		return Config{}, err
	}

	if cfgFile != "" {
		fc, err := loadFile(cfgFile)
		if err != nil {
			return Config{}, err
		}

		listen = overlay(set, "listen", listen, fc.ListenAddr)
		algorithm = overlay(set, "algorithm", algorithm, fc.Algorithm)
		breakerThreshold = overlay(set, "breaker_threshold", breakerThreshold, fc.BreakerThreshold)

		if !set["backends"] && fc.Backends != nil {
			backends = fc.Backends
		}
		if !set["weights"] && fc.Weights != nil {
			weights = fc.Weights
		}

		if healthCheckEvery, err = overlayDuration(set, "health_check_every", healthCheckEvery, fc.HealthCheckEvery); err != nil {
			return Config{}, err
		}
		if healthCheckTimeout, err = overlayDuration(set, "health_check_timeout", healthCheckTimeout, fc.HealthCheckTimeout); err != nil {
			return Config{}, err
		}
		if breakerCooldown, err = overlayDuration(set, "breaker_cooldown", breakerCooldown, fc.BreakerCooldown); err != nil {
			return Config{}, err
		}
		logFormat = overlay(set, "log-format", logFormat, fc.LogFormat)
		logLevel = overlay(set, "log-level", logLevel, fc.LogLevel)
		maxBodyBytes = overlay(set, "max-body-bytes", maxBodyBytes, fc.MaxBodyBytes)
		maxConcurrent = overlay(set, "max-concurrent", maxConcurrent, fc.MaxConcurrent)
	}

	cfg := Config{
		ListenAddr:         listen,
		Backends:           backends,
		HealthCheckEvery:   healthCheckEvery,
		HealthCheckTimeout: healthCheckTimeout,
		Algorithm:          algorithm,
		Weights:            weights,
		BreakerThreshold:   breakerThreshold,
		BreakerCooldown:    breakerCooldown,
		LogFormat:          logFormat,
		LogLevel:           logLevel,
		MaxBodyBytes:       maxBodyBytes,
		MaxConcurrent:      maxConcurrent,
	}

	if cfg.ListenAddr == "" {
		return Config{}, fmt.Errorf("listen address must not be empty")
	}
	if len(cfg.Backends) == 0 {
		return Config{}, fmt.Errorf("at least one backend is required")
	}
	if !validAlgorithm(cfg.Algorithm) {
		return Config{}, fmt.Errorf("unknown algorithm %q (allowed: round-robin, weighted-round-robin, least-connections)", cfg.Algorithm)
	}
	if cfg.BreakerThreshold < 1 {
		return Config{}, fmt.Errorf("breaker-threshold must be >= 1")
	}
	if cfg.BreakerCooldown <= 0 {
		return Config{}, fmt.Errorf("breaker-cooldown must be positive")
	}
	if len(cfg.Weights) > 0 && len(cfg.Weights) != len(cfg.Backends) {
		return Config{}, fmt.Errorf("weights count (%d) must match backends count (%d)", len(cfg.Weights), len(cfg.Backends))
	}
	switch cfg.LogFormat {
	case "text", "json":
	default:
		return Config{}, fmt.Errorf("unknown log format %q (allowed: text, json)", cfg.LogFormat)
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		return Config{}, fmt.Errorf("unknown log level %q (allowed: debug, info, warn, error)", cfg.LogLevel)
	}
	if cfg.MaxBodyBytes < 0 {
		return Config{}, fmt.Errorf("max-body-bytes must not be negative")
	}
	if cfg.MaxConcurrent < 0 {
		return Config{}, fmt.Errorf("max-concurrent must not be negative")
	}

	return cfg, nil
}

// overlay returns file when the flag was not explicitly set and the file
// defines a value; otherwise it returns the flag value (or its default).
func overlay[T any](set map[string]bool, flagName string, flagValue T, file *T) T {
	if !set[flagName] && file != nil {
		return *file
	}
	return flagValue
}

// overlayDuration is overlay for duration-typed fields, whose file values
// are strings parsed with time.ParseDuration.
func overlayDuration(set map[string]bool, flagName string, flagValue time.Duration, file *string) (time.Duration, error) {
	if set[flagName] || file == nil {
		return flagValue, nil
	}
	d, err := time.ParseDuration(*file)
	if err != nil {
		return 0, fmt.Errorf("invalid %s %q: %w", flagName, *file, err)
	}
	return d, nil
}

// loadFile reads a YAML (.yaml/.yml) or JSON (.json) config file, detected
// by extension.
func loadFile(path string) (*fileConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	fc := new(fileConfig)
	switch {
	case strings.HasSuffix(strings.ToLower(path), ".json"):
		err = json.Unmarshal(data, fc)
	case strings.HasSuffix(strings.ToLower(path), ".yaml"), strings.HasSuffix(strings.ToLower(path), ".yml"):
		err = yaml.Unmarshal(data, fc)
	default:
		return nil, fmt.Errorf("unsupported config file extension for %q (use .yaml, .yml or .json)", path)
	}
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return fc, nil
}

func splitCSV(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseWeights(weightsStr string) ([]int, error) {
	if weightsStr == "" {
		return nil, nil
	}
	var weights []int
	for part := range strings.SplitSeq(weightsStr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		w, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("invalid weight %q: %w", part, err)
		}
		weights = append(weights, w)
	}
	return weights, nil
}

func validAlgorithm(name string) bool {
	switch name {
	case "round-robin", "weighted-round-robin", "least-connections":
		return true
	}
	return false
}
