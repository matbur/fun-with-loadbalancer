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
	// defaultParallelism is how many backends one cycle probes at once. Any
	// value works; this one keeps a cycle near a single timeout for pool sizes
	// well past what a demo needs, without letting a large pool open a
	// connection per backend every interval.
	defaultParallelism = 16

	failureThreshold = 3
	successThreshold = 2
)

type Checker struct {
	pool     *balancer.Pool
	client   *http.Client
	interval time.Duration
	timeout  time.Duration
	// parallelism caps how many probes one cycle runs at once (0 = all).
	parallelism int

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
		pool:        pool,
		client:      &http.Client{},
		interval:    defaultInterval,
		timeout:     defaultTimeout,
		parallelism: defaultParallelism,
		state:       make(map[*balancer.Backend]*backendState),
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

// WithParallelism caps how many backends one cycle probes at once. 0 means no
// cap. The cap is what keeps -health-check-every the real interval: a cycle that
// takes longer than the interval makes the ticker drop ticks, so the effective
// interval becomes the cycle duration. See checkAll.
func WithParallelism(n int) Option {
	return func(c *Checker) { c.parallelism = max(n, 0) }
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

// checkAll probes every backend once, several at a time, then applies the
// results.
//
// The fan-out is what keeps the configured interval meaningful. Probing one
// backend at a time costs up to len(backends) * timeout, and a time.Ticker drops
// ticks while a cycle is in progress — so once the cycle outlasts the interval
// the effective interval becomes the cycle duration, and the last backend in the
// list is not even looked at until every earlier one has timed out. Probing
// concurrently brings a cycle back to roughly one timeout, bounded by
// parallelism so a large pool does not open a connection per backend at once.
//
// Results are applied after the fan-out rather than as each probe returns, so
// the state transitions of one cycle do not interleave with the next.
func (c *Checker) checkAll(ctx context.Context) {
	backends := c.pool.Backends()
	results := make([]bool, len(backends))

	// Acquire the permit before starting the goroutine, so at most parallelism
	// probes exist at a time instead of one goroutine per backend.
	sem := make(chan struct{}, c.limit(len(backends)))
	var wg sync.WaitGroup
	for i, b := range backends {
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = c.checkBackend(ctx, b)
		})
	}
	wg.Wait()

	for i, b := range backends {
		c.update(b, results[i])
	}
	c.prune(backends)
}

// limit returns the effective probe bound for a pool of n backends: the
// configured parallelism, or n when it is uncapped.
func (c *Checker) limit(n int) int {
	if c.parallelism == 0 {
		return n
	}
	return min(c.parallelism, n)
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
			// A backend that answers its health endpoint is reachable, so a
			// circuit left open by request traffic has nothing left to protect
			// against. Same hysteresis as Healthy above, so the two cannot
			// disagree; without it an idle load balancer has no way back, since
			// no request ever resolves half-open.
			b.ReportHealthy()
		}
	default:
		st.consecutiveSuccess = 0
		st.consecutiveFailures++
		if st.consecutiveFailures >= failureThreshold {
			b.Healthy.Store(false)
		}
	}
}
