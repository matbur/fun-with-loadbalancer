package balancer

import (
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type Backend struct {
	URL     *url.URL
	Proxy   *httputil.ReverseProxy
	Healthy atomic.Bool
}

type BackendOption func(*Backend)

func WithTransport(rt http.RoundTripper) BackendOption {
	return func(b *Backend) {
		b.Proxy.Transport = rt
	}
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
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			w.Header().Set("X-Content-Type-Options", "nosniff")
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
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
