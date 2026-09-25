package proxy

import (
	"log/slog"
	"net/http"
	"time"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/metrics"
)

const maxAttempts = 2

type Server struct {
	pool    *balancer.Pool
	metrics *metrics.Metrics
	logger  *slog.Logger
}

type Option func(*Server)

// WithMetrics enables per-backend request/error accounting for the server.
func WithMetrics(m *metrics.Metrics) Option {
	return func(s *Server) {
		s.metrics = m
	}
}

// WithLogger enables structured access logging for the server. Without it
// the server logs nothing.
func WithLogger(l *slog.Logger) Option {
	return func(s *Server) {
		s.logger = l
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
		s.logAccess(r, http.StatusServiceUnavailable, 0, "")
		return
	}

	start := time.Now()
	s.observeBackend(backend)
	backend.Acquire()
	defer backend.Release()

	tw := newTrackingWriter(w)
	backend.Proxy.ServeHTTP(tw, r)
	elapsed := time.Since(start)

	if tw.err == nil {
		backend.ReportSuccess()
		s.logAccess(r, tw.status, elapsed, backend.URL.Host)
		return
	}
	backend.ReportFailure()
	s.observeBackendError(backend)
	if tw.wroteHeader {
		s.logAccess(r, tw.status, elapsed, backend.URL.Host)
		return
	}
	status := balancer.StatusCodeForError(tw.err)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, http.StatusText(status), status)
	s.logAccess(r, status, elapsed, backend.URL.Host)
}

func (s *Server) serveWithRetry(w http.ResponseWriter, r *http.Request) {
	tw := newTrackingWriter(w)
	var lastErr error
	var served string
	start := time.Now()

	for range maxAttempts {
		backend := s.pool.Next()
		if backend == nil {
			http.Error(w, "no healthy backends", http.StatusServiceUnavailable)
			s.logAccess(r, http.StatusServiceUnavailable, time.Since(start), "")
			return
		}
		served = backend.URL.Host

		s.observeBackend(backend)
		backend.Acquire()
		tw.reset()
		backend.Proxy.ServeHTTP(tw, r)
		backend.Release()

		if tw.err == nil {
			backend.ReportSuccess()
			s.logAccess(r, tw.status, time.Since(start), served)
			return
		}
		backend.ReportFailure()
		s.observeBackendError(backend)
		lastErr = tw.err
		if tw.wroteHeader {
			s.logAccess(r, tw.status, time.Since(start), served)
			return
		}
	}

	status := balancer.StatusCodeForError(lastErr)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, http.StatusText(status), status)
	s.logAccess(r, status, time.Since(start), served)
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

// logAccess emits one structured line per request — method, path, status,
// duration and the backend that served it. Responses >= 500 are logged at
// Warn level, everything else at Info. It is a no-op unless a logger was
// configured with WithLogger.
func (s *Server) logAccess(r *http.Request, status int, duration time.Duration, backend string) {
	if s.logger == nil {
		return
	}
	attrs := []any{
		"method", r.Method,
		"path", r.URL.Path,
		"status", status,
		"duration", duration,
		"remote", r.RemoteAddr,
	}
	if backend != "" {
		attrs = append(attrs, "backend", backend)
	}
	if status >= http.StatusInternalServerError {
		s.logger.Warn("request failed", attrs...)
		return
	}
	s.logger.Info("request", attrs...)
}
