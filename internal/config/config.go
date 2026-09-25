package config

import (
	"flag"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr         string
	Backends           []string
	HealthCheckEvery   time.Duration
	HealthCheckTimeout time.Duration

	// Selection algorithm: one of round-robin, weighted-round-robin,
	// least-connections (validated by cmd/lb, which maps it to balancer).
	Algorithm string
	// Weights aligned with Backends; empty means 1 for every backend.
	Weights []int

	BreakerThreshold int
	BreakerCooldown  time.Duration
}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("lb", flag.ContinueOnError)

	var listen string
	var backendsStr string
	var healthCheckEvery time.Duration
	var healthCheckTimeout time.Duration
	var algorithm string
	var weightsStr string
	var breakerThreshold int
	var breakerCooldown time.Duration

	fs.StringVar(&listen, "listen", ":8080", "address to listen on")
	fs.StringVar(&backendsStr, "backends", "", "comma-separated backend URLs")
	fs.DurationVar(&healthCheckEvery, "health-check-every", 10*time.Second, "health check interval")
	fs.DurationVar(&healthCheckTimeout, "health-check-timeout", 2*time.Second, "health check per-backend timeout")
	fs.StringVar(&algorithm, "algorithm", "round-robin", "selection algorithm: round-robin | weighted-round-robin | least-connections")
	fs.StringVar(&weightsStr, "weights", "", "comma-separated backend weights (must match -backends count)")
	fs.IntVar(&breakerThreshold, "breaker-threshold", 3, "consecutive transport failures before a backend's circuit opens")
	fs.DurationVar(&breakerCooldown, "breaker-cooldown", 5*time.Second, "how long an open circuit stays open")

	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}

	var backends []string
	for rawURL := range strings.SplitSeq(backendsStr, ",") {
		rawURL = strings.TrimSpace(rawURL)
		if rawURL != "" {
			backends = append(backends, rawURL)
		}
	}

	var weights []int
	if weightsStr != "" {
		for part := range strings.SplitSeq(weightsStr, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			w, err := strconv.Atoi(part)
			if err != nil {
				return Config{}, fmt.Errorf("invalid weight %q: %w", part, err)
			}
			weights = append(weights, w)
		}
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
	}

	if cfg.ListenAddr == "" {
		return Config{}, fmt.Errorf("listen address must not be empty")
	}
	if len(cfg.Backends) == 0 {
		return Config{}, fmt.Errorf("at least one backend is required")
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

	return cfg, nil
}
