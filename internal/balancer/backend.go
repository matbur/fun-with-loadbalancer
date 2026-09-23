package balancer

import (
	"net/http/httputil"
	"net/url"
	"sync/atomic"
)

type Backend struct {
	URL     *url.URL
	Proxy   *httputil.ReverseProxy
	Healthy atomic.Bool
}
