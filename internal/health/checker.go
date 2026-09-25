package health

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"

	"fun-with-loadbalancer/internal/balancer"
)

const (
	defaultInterval = 10 * time.Second
	defaultTimeout  = 2 * time.Second

	failureThreshold = 3
	successThreshold = 2
)

type Checker struct {
	pool     *balancer.Pool
	client   *http.Client
	interval time.Duration
	timeout  time.Duration

	mu    sync.Mutex
	state map[*balancer.Backend]*backendState
}

type backendState struct {
	consecutiveFailures int
	consecutiveSuccess  int
}

type Option func(*Checker)

func New(pool *balancer.Pool, opts ...Option) *Checker {
	c := &Checker{
		pool:     pool,
		client:   &http.Client{},
		interval: defaultInterval,
		timeout:  defaultTimeout,
		state:    make(map[*balancer.Backend]*backendState),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

func WithInterval(d time.Duration) Option {
	return func(c *Checker) { c.interval = d }
}

func WithTimeout(d time.Duration) Option {
	return func(c *Checker) { c.timeout = d }
}

func WithTransport(rt http.RoundTripper) Option {
	return func(c *Checker) { c.client.Transport = rt }
}

func (c *Checker) Run(ctx context.Context) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	c.checkAll(ctx)

	for {
		select {
		case <-ticker.C:
			c.checkAll(ctx)
		case <-ctx.Done():
			return
		}
	}
}

func (c *Checker) checkAll(ctx context.Context) {
	backends := c.pool.Backends()
	for _, b := range backends {
		c.update(b, c.checkBackend(ctx, b))
	}
	c.prune(backends)
}

// prune drops health state for backends that are no longer in the pool, so a
// removed backend does not leak state and a re-added one starts fresh.
func (c *Checker) prune(current []*balancer.Backend) {
	keep := make(map[*balancer.Backend]struct{}, len(current))
	for _, b := range current {
		keep[b] = struct{}{}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for b := range c.state {
		if _, ok := keep[b]; !ok {
			delete(c.state, b)
		}
	}
}

func (c *Checker) checkBackend(ctx context.Context, b *balancer.Backend) bool {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		b.URL.ResolveReference(&url.URL{Path: "/healthz"}).String(),
		nil,
	)
	if err != nil {
		return false
	}

	resp, err := c.client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

func (c *Checker) update(b *balancer.Backend, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.state[b]
	if st == nil {
		st = &backendState{}
		c.state[b] = st
	}

	switch {
	case ok:
		st.consecutiveFailures = 0
		st.consecutiveSuccess++
		if st.consecutiveSuccess >= successThreshold {
			b.Healthy.Store(true)
		}
	default:
		st.consecutiveSuccess = 0
		st.consecutiveFailures++
		if st.consecutiveFailures >= failureThreshold {
			b.Healthy.Store(false)
		}
	}
}
