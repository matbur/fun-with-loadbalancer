package proxy

import (
	"errors"
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
	// maxBodyBytes caps the request body forwarded upstream in bytes.
	// 0 means unlimited.
	maxBodyBytes int64
	// sem is a token pool limiting in-flight requests. nil means unlimited.
	sem chan struct{}
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

// WithMaxBodyBytes caps how much of the request body the server forwards
// upstream, in bytes (0 = unlimited, the default). Requests that exceed the
// limit get 413 before a backend is touched.
func WithMaxBodyBytes(n int64) Option {
	return func(s *Server) {
		s.maxBodyBytes = max(n, 0)
	}
}

// WithMaxConcurrent caps the number of in-flight requests the server
// processes at once (0 = unlimited, the default). Requests beyond the cap
// are rejected with 503 immediately, without touching a backend.
func WithMaxConcurrent(n int) Option {
	return func(s *Server) {
		if n > 0 {
			s.sem = make(chan struct{}, n)
		}
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
	if s.maxBodyBytes > 0 && !s.limitRequestBody(w, r) {
		return
	}
	if s.sem != nil && !s.acquireSlot() {
		http.Error(w, "load balancer at capacity", http.StatusServiceUnavailable)
		s.logAccess(r, http.StatusServiceUnavailable, 0, "")
		return
	}
	if s.sem != nil {
		defer s.releaseSlot()
	}

	if isRetryable(r.Method) {
		s.serveWithRetry(w, r)
		return
	}
	s.serveDirect(w, r)
}

// limitRequestBody rejects requests whose declared Content-Length already
// exceeds the limit and wraps the body so oversized transfers (e.g. chunked)
// fail with *http.MaxBytesError, which serveDirect/serveWithRetry map to 413.
func (s *Server) limitRequestBody(w http.ResponseWriter, r *http.Request) bool {
	if r.ContentLength > s.maxBodyBytes {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		s.logAccess(r, http.StatusRequestEntityTooLarge, 0, "")
		return false
	}
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	return true
}

func (s *Server) acquireSlot() bool {
	select {
	case s.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) releaseSlot() {
	<-s.sem
}

// isBodyTooLarge reports whether the upstream transfer failed because the
// client sent more body than allowed. Such failures are client-caused: they
// must not trip the circuit breaker nor trigger a retry.
func isBodyTooLarge(err error) bool {
	var maxBytesErr *http.MaxBytesError
	return errors.As(err, &maxBytesErr)
}

// writeStatus writes a plain HTTP error response like http.Error does, but
// with the nosniff header the proxy sets on every error it generates itself.
func writeStatus(w http.ResponseWriter, status int) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, http.StatusText(status), status)
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
	tw := newTrackingWriter(w)
	s.serveOn(backend, tw, r)
	elapsed := time.Since(start)

	if tw.err == nil {
		backend.ReportSuccess()
		s.logAccess(r, tw.status, elapsed, backend.URL.Host)
		return
	}
	if isBodyTooLarge(tw.err) {
		// Client-caused: not a backend failure, no breaker trip.
		writeStatus(w, http.StatusRequestEntityTooLarge)
		s.logAccess(r, http.StatusRequestEntityTooLarge, elapsed, backend.URL.Host)
		return
	}
	backend.ReportFailure()
	s.observeBackendError(backend)
	if tw.wroteHeader {
		s.logAccess(r, tw.status, elapsed, backend.URL.Host)
		return
	}
	status := balancer.StatusCodeForError(tw.err)
	writeStatus(w, status)
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

		s.serveOn(backend, tw, r)

		if tw.err == nil {
			backend.ReportSuccess()
			s.logAccess(r, tw.status, time.Since(start), served)
			return
		}
		if isBodyTooLarge(tw.err) {
			// Client-caused: no retry, no breaker trip.
			writeStatus(w, http.StatusRequestEntityTooLarge)
			s.logAccess(r, http.StatusRequestEntityTooLarge, time.Since(start), served)
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
	writeStatus(w, status)
	s.logAccess(r, status, time.Since(start), served)
}

// serveOn proxies r to backend through tw, holding one in-flight slot on that
// backend for the duration. The release is deferred on purpose: when the
// response copy fails mid-body — a client that hangs up, an upstream that
// resets the connection — ReverseProxy panics with http.ErrAbortHandler and
// unwinds through here, and the slot must be given back all the same.
// httptest aside, only the real server recovers that panic, so the leak would
// otherwise be invisible in unit tests and permanent in production.
func (s *Server) serveOn(backend *balancer.Backend, tw *trackingWriter, r *http.Request) {
	s.observeBackend(backend)
	backend.Acquire()
	defer backend.Release()

	tw.reset()
	backend.Proxy.ServeHTTP(tw, r)
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
