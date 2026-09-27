package main

import (
	"context"
	"errors"
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

// newHTTPServer builds the HTTP server from config.
//
// ReadTimeout and WriteTimeout default to 0, i.e. no deadline, because the
// request limits this LB documents are size-based: a body is bounded by
// -max-body-bytes, not by a clock. A whole-request deadline shorter than the
// time a client needs to send -max-body-bytes pre-empts the documented 413, and
// the resulting error is charged to the backend as a transport failure — see
// finding 7.
//
// ReadHeaderTimeout and IdleTimeout stay hardcoded: they bound connection
// lifetime (slowloris, idle keep-alive) and cannot contradict documented request
// semantics the way a whole-request deadline can.
func newHTTPServer(cfg config.Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadTimeout:       cfg.ReadTimeout,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       60 * time.Second,
	}
}

// newLogger builds the process logger from the validated config values: a
// JSON handler when format is "json", a text handler otherwise, both with
// the configured minimum level. Invalid values are rejected by config.Parse,
// so unknown inputs fall back to their defaults here.
func newLogger(format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slogLevel(level)}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// slogLevel maps the config level name to slog.Level, falling back to info.
func slogLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default: // "info"
		return slog.LevelInfo
	}
}

// serveUntilShutdown serves srv on ln until ctx is canceled (SIGINT/SIGTERM
// in main), then shuts the server down gracefully and blocks until the
// in-flight requests finished or shutdownTimeout elapsed. It returns nil
// after a graceful shutdown and the serve error when the server failed on
// its own.
//
// Waiting on the drain matters: Shutdown first closes the listeners, which
// makes Serve return http.ErrServerClosed immediately, and only then waits
// for in-flight requests. Without the shutdownDone wait, the caller would
// return right after Serve and the process would exit while requests were
// still being handled.
func serveUntilShutdown(ctx context.Context, ln net.Listener, srv *http.Server, logger *slog.Logger) error {
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
		close(shutdownDone)
	}()

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
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
		"health_check_parallelism", cfg.HealthCheckParallelism,
		"breaker_threshold", cfg.BreakerThreshold,
		"breaker_cooldown", cfg.BreakerCooldown,
		"read_timeout", cfg.ReadTimeout,
		"write_timeout", cfg.WriteTimeout,
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
		health.WithParallelism(cfg.HealthCheckParallelism),
		health.WithTransport(transport),
	)
	go checker.Run(ctx)

	admin := newAdminAPI(pool, transport, cfg.BreakerThreshold, cfg.BreakerCooldown)

	mux := newMux(pool, admin, m, m.Wrap(proxy.NewServer(pool,
		proxy.WithMetrics(m),
		proxy.WithLogger(logger),
		proxy.WithMaxBodyBytes(cfg.MaxBodyBytes),
		proxy.WithMaxConcurrent(cfg.MaxConcurrent))))

	httpServer := newHTTPServer(cfg, mux)

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
