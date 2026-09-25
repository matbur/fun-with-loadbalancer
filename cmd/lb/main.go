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

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	transport := balancer.NewTransport()
	m := metrics.New()

	backends := make([]*balancer.Backend, 0, len(cfg.Backends))
	for _, rawURL := range cfg.Backends {
		b, err := balancer.NewBackend(rawURL, balancer.WithTransport(transport))
		if err != nil {
			log.Fatalf("backend %q: %v", rawURL, err)
		}
		backends = append(backends, b)
	}
	pool := balancer.NewPool(backends)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	checker := health.New(
		pool,
		health.WithInterval(cfg.HealthCheckEvery),
		health.WithTimeout(cfg.HealthCheckTimeout),
		health.WithTransport(transport),
	)
	go checker.Run(ctx)

	mux := http.NewServeMux()
	mux.Handle("/metrics", m.Handler())
	mux.Handle("/", m.Wrap(proxy.NewServer(pool, proxy.WithMetrics(m))))

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
