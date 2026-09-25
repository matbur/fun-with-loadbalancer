package health

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
)

func newToggleServer(healthy *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if healthy.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.Error(w, "nope", http.StatusServiceUnavailable)
	}))
}

func mustBackend(t *testing.T, rawURL string) *balancer.Backend {
	t.Helper()
	b, err := balancer.NewBackend(rawURL)
	require.NoError(t, err)
	return b
}

func TestCheckerWithTransport(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := newToggleServer(&healthy)
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	tr := balancer.NewTransport()
	c := New(pool, WithTransport(tr))

	assert.Same(t, tr, c.client.Transport)
}

func TestCheckerStaysHealthyWhenBackendUp(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := newToggleServer(&healthy)
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	c := New(pool)
	c.checkAll(context.Background())

	assert.True(t, b.Healthy.Load())
}

func TestCheckerMarksUnhealthyAfterFailureThreshold(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := newToggleServer(&healthy)
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	c := New(pool)

	healthy.Store(false)
	c.checkAll(context.Background())
	assert.True(t, b.Healthy.Load(), "single failure must not flip state")

	c.checkAll(context.Background())
	c.checkAll(context.Background())
	assert.False(t, b.Healthy.Load(), "backend should be unhealthy after failureThreshold failures")
}

func TestCheckerRecoversAfterSuccessThreshold(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(false)
	srv := newToggleServer(&healthy)
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	c := New(pool)
	for range failureThreshold {
		c.checkAll(context.Background())
	}
	assert.False(t, b.Healthy.Load())

	healthy.Store(true)
	c.checkAll(context.Background())
	assert.False(t, b.Healthy.Load(), "single success must not flip state")

	c.checkAll(context.Background())
	assert.True(t, b.Healthy.Load(), "backend should be healthy after successThreshold successes")
}

func TestCheckerTimeoutDoesNotBlock(t *testing.T) {
	slowSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(5 * time.Second):
		case <-r.Context().Done():
		}
	}))
	defer slowSrv.Close()

	b := mustBackend(t, slowSrv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	c := New(pool, WithTimeout(50*time.Millisecond))

	start := time.Now()
	c.checkAll(context.Background())

	assert.Less(t, time.Since(start), time.Second)
	assert.True(t, b.Healthy.Load(), "single timeout must not flip state")
}

func TestCheckerRunStopsOnContextCancel(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := newToggleServer(&healthy)
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	c := New(pool, WithInterval(time.Hour))

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		c.Run(ctx)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancellation")
	}
}

func TestCheckerRunTogglesHealth(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	srv := newToggleServer(&healthy)
	defer srv.Close()

	b := mustBackend(t, srv.URL)
	pool := balancer.NewPool([]*balancer.Backend{b})

	c := New(pool, WithInterval(20*time.Millisecond), WithTimeout(200*time.Millisecond))

	ctx := t.Context()
	go c.Run(ctx)

	healthy.Store(false)
	assert.Eventually(t, func() bool { return !b.Healthy.Load() }, 2*time.Second, 10*time.Millisecond)

	healthy.Store(true)
	assert.Eventually(t, func() bool { return b.Healthy.Load() }, 2*time.Second, 10*time.Millisecond)
}