package config

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

type Config struct {
	ListenAddr       string
	Backends         []string
	HealthCheckEvery time.Duration
}

func Parse(args []string) (Config, error) {
	fs := flag.NewFlagSet("lb", flag.ContinueOnError)

	var listen string
	var backendsStr string
	var healthCheckEvery time.Duration

	fs.StringVar(&listen, "listen", ":8080", "address to listen on")
	fs.StringVar(&backendsStr, "backends", "", "comma-separated backend URLs")
	fs.DurationVar(&healthCheckEvery, "health-check-every", 10*time.Second, "health check interval")

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

	cfg := Config{
		ListenAddr:       listen,
		Backends:         backends,
		HealthCheckEvery: healthCheckEvery,
	}

	if cfg.ListenAddr == "" {
		return Config{}, fmt.Errorf("listen address must not be empty")
	}
	if len(cfg.Backends) == 0 {
		return Config{}, fmt.Errorf("at least one backend is required")
	}

	return cfg, nil
}
