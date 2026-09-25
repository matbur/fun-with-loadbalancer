package main

import (
	"context"
	"errors"
	"log"
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
		log.Fatalf("config: %v", err)
	}

	transport := balancer.NewTransport()
	m := metrics.New()

	backends := make([]*balancer.Backend, 0, len(cfg.Backends))
	for i, rawURL := range cfg.Backends {
		weight := 1
		if len(cfg.Weights) > 0 {
			weight = cfg.Weights[i]
		}
		b, err := balancer.NewBackend(rawURL, backendOptions(cfg, transport, weight)...)
		if err != nil {
			log.Fatalf("backend %q: %v", rawURL, err)
		}
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
	mux.Handle("/", m.Wrap(proxy.NewServer(pool, proxy.WithMetrics(m))))
	mux.HandleFunc("GET /backends", admin.list)
	mux.HandleFunc("POST /backends", admin.add)
	mux.HandleFunc("DELETE /backends", admin.remove)

	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Print("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server: %v", err)
	}
}
