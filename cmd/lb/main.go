package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/config"
	"fun-with-loadbalancer/internal/health"
	"fun-with-loadbalancer/internal/metrics"
	"fun-with-loadbalancer/internal/proxy"
)

const shutdownTimeout = 10 * time.Second

// algorithmFromConfig maps the validated -algorithm flag to the pool
// algorithm. config.Parse allows only the three documented names.
func algorithmFromConfig(name string) balancer.Algorithm {
	switch name {
	case "weighted-round-robin":
		return balancer.AlgorithmWeightedRoundRobin
	case "least-connections":
		return balancer.AlgorithmLeastConnections
	default: // "round-robin"
		return balancer.AlgorithmRoundRobin
	}
}

// backendOptions builds the options shared by every backend: the shared
// transport, weight and circuit breaker parameters.
func backendOptions(cfg config.Config, transport *http.Transport, weight int) []balancer.BackendOption {
	return []balancer.BackendOption{
		balancer.WithTransport(transport),
		balancer.WithWeight(weight),
		balancer.WithBreakerThreshold(cfg.BreakerThreshold),
		balancer.WithBreakerCooldown(cfg.BreakerCooldown),
	}
}

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(1)
	}

	logger := newLogger(cfg.LogFormat, cfg.LogLevel)
	slog.SetDefault(logger)

	transport := balancer.NewTransport()
	m := metrics.New()

	logger.Info("starting load balancer",
		"listen", cfg.ListenAddr,
		"algorithm", cfg.Algorithm,
		"backends", len(cfg.Backends),
		"health_check_every", cfg.HealthCheckEvery,
		"health_check_timeout", cfg.HealthCheckTimeout,
		"breaker_threshold", cfg.BreakerThreshold,
		"breaker_cooldown", cfg.BreakerCooldown,
	)

	backends := make([]*balancer.Backend, 0, len(cfg.Backends))
	for i, rawURL := range cfg.Backends {
		weight := 1
		if len(cfg.Weights) > 0 {
			weight = cfg.Weights[i]
		}
		b, err := balancer.NewBackend(rawURL, backendOptions(cfg, transport, weight)...)
		if err != nil {
			logger.Error("invalid backend", "url", rawURL, "error", err)
			os.Exit(1)
		}
		logger.Debug("backend registered", "url", rawURL, "weight", weight)
		backends = append(backends, b)
	}
	pool := balancer.NewPool(backends, balancer.WithAlgorithm(algorithmFromConfig(cfg.Algorithm)))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	checker := health.New(
		pool,
		health.WithInterval(cfg.HealthCheckEvery),
		health.WithTimeout(cfg.HealthCheckTimeout),
		health.WithTransport(transport),
	)
	go checker.Run(ctx)

	admin := newAdminAPI(pool, transport, cfg.BreakerThreshold, cfg.BreakerCooldown)

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.Handle("/", m.Wrap(proxy.NewServer(pool,
		proxy.WithMetrics(m),
		proxy.WithLogger(logger),
		proxy.WithMaxBodyBytes(cfg.MaxBodyBytes),
		proxy.WithMaxConcurrent(cfg.MaxConcurrent))))
	mux.HandleFunc("GET /backends", admin.list)
	mux.HandleFunc("POST /backends", admin.add)
	mux.HandleFunc("DELETE /backends", admin.remove)
	mux.HandleFunc("GET /live", handleLive)
	mux.HandleFunc("GET /ready", handleReady(pool))

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	logger.Info("listening", "addr", cfg.ListenAddr)

	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		logger.Error("listen failed", "addr", cfg.ListenAddr, "error", err)
		os.Exit(1)
	}

	if err := serveUntilShutdown(ctx, ln, httpServer, logger); err != nil {
		logger.Error("server error", "error", err)
		os.Exit(1)
	}
	transport.CloseIdleConnections()
}
