package balancer

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type Backend struct {
	URL     *url.URL
	Proxy   *httputil.ReverseProxy
	Healthy atomic.Bool

	weight int          // relative weight for weighted algorithms (default 1)
	active atomic.Int32 // in-flight requests for least-connections
}

type BackendOption func(*Backend)

func WithTransport(rt http.RoundTripper) BackendOption {
	return func(b *Backend) {
		b.Proxy.Transport = rt
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

type upstreamErrorWriter interface {
	UpstreamError(error)
}

func StatusCodeForError(err error) int {
	if isTimeout(err) {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

func isTimeout(err error) bool {
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
	}
	for _, opt := range opts {
		opt(b)
	}

	b.Healthy.Store(true)

	return b, nil
}
