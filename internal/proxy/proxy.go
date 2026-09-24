package proxy

import (
	"net/http"

	"fun-with-loadbalancer/internal/balancer"
)

const maxAttempts = 2

type Server struct {
	pool *balancer.Pool
}

func NewServer(pool *balancer.Pool) *Server {
	return &Server{pool: pool}
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if isRetryable(r.Method) {
		s.serveWithRetry(w, r)
		return
	}
	s.serveDirect(w, r)
}

func isRetryable(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

func (s *Server) serveDirect(w http.ResponseWriter, r *http.Request) {
	backend := s.pool.Next()
	if backend == nil {
		http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
		return
	}

	backend.Proxy.ServeHTTP(w, r)
}

func (s *Server) serveWithRetry(w http.ResponseWriter, r *http.Request) {
	tw := newTrackingWriter(w)
	var lastErr error

	for range maxAttempts {
		backend := s.pool.Next()
		if backend == nil {
			http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
			return
		}

		tw.reset()
		backend.Proxy.ServeHTTP(tw, r)

		if tw.err == nil {
			return
		}
		lastErr = tw.err
		if tw.wroteHeader {
			return
		}
	}

	status := balancer.StatusCodeForError(lastErr)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, http.StatusText(status), status)
}
