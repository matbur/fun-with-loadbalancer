package proxy

import (
	"net/http"

	"fun-with-loadbalancer/internal/balancer"
)

type Server struct {
	pool *balancer.Pool
}

func NewServer(pool *balancer.Pool) *Server {
	return &Server{pool: pool}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	backend := s.pool.Next()
	if backend == nil {
		http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
		return
	}

	backend.Proxy.ServeHTTP(w, r)
}