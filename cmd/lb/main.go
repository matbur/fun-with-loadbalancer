package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/config"
	"fun-with-loadbalancer/internal/health"
	"fun-with-loadbalancer/internal/proxy"
)

func main() {
	cfg, err := config.Parse(os.Args[1:])
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	backends := make([]*balancer.Backend, 0, len(cfg.Backends))
	for _, rawURL := range cfg.Backends {
		b, err := balancer.NewBackend(rawURL)
		if err != nil {
			log.Fatalf("backend %q: %v", rawURL, err)
		}
		backends = append(backends, b)
	}

	pool := balancer.NewPool(backends)

	checker := health.New(
		pool,
		health.WithInterval(cfg.HealthCheckEvery),
		health.WithTimeout(cfg.HealthCheckTimeout),
	)
	go checker.Run(context.Background())

	server := proxy.NewServer(pool)

	log.Printf("listening on %s", cfg.ListenAddr)
	if err := http.ListenAndServe(cfg.ListenAddr, server); err != nil {
		log.Fatalf("server: %v", err)
	}
}