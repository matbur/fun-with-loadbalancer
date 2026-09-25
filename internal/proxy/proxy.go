package proxy

import (
	"net/http"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/metrics"
)

const maxAttempts = 2

type Server struct {
	pool    *balancer.Pool
	metrics *metrics.Metrics
}

type Option func(*Server)

// WithMetrics enables per-backend request/error accounting for the server.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Server) {
		s.metrics = m
	}
}

func NewServer(pool *balancer.Pool, opts ...Option) *Server {
	s := &Server{pool: pool}
	for _, opt := range opts {
		opt(s)
	}
	return s
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

	s.observeBackend(backend)
	backend.Acquire()
	defer backend.Release()

	tw := newTrackingWriter(w)
	backend.Proxy.ServeHTTP(tw, r)

	if tw.err == nil {
		backend.ReportSuccess()
		return
	}
	backend.ReportFailure()
	s.observeBackendError(backend)
	if tw.wroteHeader {
		return
	}
	status := balancer.StatusCodeForError(tw.err)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, http.StatusText(status), status)
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

		s.observeBackend(backend)
		backend.Acquire()
		tw.reset()
		backend.Proxy.ServeHTTP(tw, r)
		backend.Release()

		if tw.err == nil {
			backend.ReportSuccess()
			return
		}
		backend.ReportFailure()
		s.observeBackendError(backend)
		lastErr = tw.err
		if tw.wroteHeader {
			return
		}
	}

	status := balancer.StatusCodeForError(lastErr)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, http.StatusText(status), status)
}

func (s *Server) observeBackend(b *balancer.Backend) {
	if s.metrics == nil {
		return
	}
	s.metrics.ObserveBackendRequest(b.URL.Host)
}

func (s *Server) observeBackendError(b *balancer.Backend) {
	if s.metrics == nil {
		return
	}
	s.metrics.ObserveBackendError(b.URL.Host)
}
