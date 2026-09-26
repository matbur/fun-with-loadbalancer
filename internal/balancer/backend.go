package balancer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"time"
)

type Backend struct {
	URL     *url.URL
	Proxy   *httputil.ReverseProxy
	Healthy atomic.Bool

	weight int          // relative weight for weighted algorithms (default 1)
	active atomic.Int32 // in-flight requests for least-connections
	// currentWeight is the smooth weighted round-robin accumulator (nginx
	// algorithm). It is only touched by Pool.nextWeighted while holding the
	// pool's mutex, so it needs no synchronization of its own.
	currentWeight int

	breaker circuitBreaker
}

type BackendOption func(*Backend)

func WithTransport(rt http.RoundTripper) BackendOption {
	return func(b *Backend) {
		b.Proxy.Transport = rt
	}
}

// WithBreakerThreshold sets how many consecutive transport-level failures
// open the backend's circuit (default 3). Values below 1 are clamped to 1.
func WithBreakerThreshold(n int) BackendOption {
	return func(b *Backend) {
		b.breaker.threshold = int32(max(n, 1))
	}
}

// WithBreakerCooldown sets how long an open circuit stays open before
// half-open probes are allowed again (default 5s).
func WithBreakerCooldown(d time.Duration) BackendOption {
	return func(b *Backend) {
		b.breaker.cooldown = d
	}
}

// WithWeight sets the relative weight used by weighted load-balancing
// algorithms. Values below 1 are clamped to 1.
func WithWeight(w int) BackendOption {
	return func(b *Backend) {
		b.weight = max(w, 1)
	}
}

// Weight returns the backend's relative weight (never below 1).
func (b *Backend) Weight() int {
	return max(b.weight, 1)
}

// Acquire marks one in-flight request on this backend. It reports to
// least-connections selection and is a no-op for other algorithms.
func (b *Backend) Acquire() {
	b.active.Add(1)
}

// Release marks the end of an in-flight request on this backend.
func (b *Backend) Release() {
	b.active.Add(-1)
}

// Active returns the number of in-flight requests currently on this backend.
func (b *Backend) Active() int {
	return int(b.active.Load())
}

// Available reports whether the backend can receive traffic: it must be healthy
// according to the health checker and its circuit must admit a request. A
// half-open circuit admits exactly one caller — the probe — until that probe
// reports, so repeated calls from concurrent requests do not all pass.
//
// The probe claim is a side effect, so callers that rank several candidates must
// expect a loser to have consumed an admission: Pool's weighted and
// least-connections scans do exactly that, leaving a recovering backend out of
// rotation until probeTimeout or ReportHealthy, whichever comes first.
// Round-robin, the default, returns the first candidate it admits and is
// unaffected.
func (b *Backend) Available() bool {
	return b.Healthy.Load() && b.breaker.allow()
}

// ReportFailure feeds a transport-level failure during request handling to
// the backend's circuit breaker.
func (b *Backend) ReportFailure() {
	b.breaker.reportFailure()
}

// ReportSuccess feeds a successfully handled request (no transport error) to
// the backend's circuit breaker.
func (b *Backend) ReportSuccess() {
	b.breaker.reportSuccess()
}

// ReportHealthy tells the breaker that an out-of-band probe (the health
// checker's /healthz request) reached the backend, recovering an open or
// half-open circuit. Unlike ReportSuccess it may close an open circuit, and it
// leaves a closed one alone — that asymmetry is what keeps a closed circuit's
// failure counter measuring request traffic only.
func (b *Backend) ReportHealthy() {
	b.breaker.recover()
}

type upstreamErrorWriter interface {
	UpstreamError(error)
}

func StatusCodeForError(err error) int {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return http.StatusRequestEntityTooLarge
	}
	if IsTimeout(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// IsTimeout reports whether err is a deadline expiry, from either the context
// or the network layer.
func IsTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

func NewBackend(rawURL string, opts ...BackendOption) (*Backend, error) {
	target, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if target.Scheme == "" || target.Host == "" {
		return nil, errors.New("backend URL must include scheme and host")
	}

	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.SetXForwarded()
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			if uew, ok := w.(upstreamErrorWriter); ok {
				uew.UpstreamError(err)
				return
			}
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.Error(w, http.StatusText(StatusCodeForError(err)), StatusCodeForError(err))
		},
	}

	b := &Backend{
		URL:   target,
		Proxy: proxy,
		breaker: circuitBreaker{
			threshold: defaultBreakerThreshold,
			cooldown:  defaultBreakerCooldown,
		},
	}
	for _, opt := range opts {
		opt(b)
	}

	b.Healthy.Store(true)

	return b, nil
}
