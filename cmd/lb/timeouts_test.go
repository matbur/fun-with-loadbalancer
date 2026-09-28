package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
	"fun-with-loadbalancer/internal/config"
	"fun-with-loadbalancer/internal/metrics"
	"fun-with-loadbalancer/internal/proxy"
)

// newSlowUploadFixture wires the production mux in front of one backend and
// starts it with srvCfg's server timeouts. It returns the server, the pool and a
// counter of the bodies the backend received in full.
func newSlowUploadFixture(t *testing.T, srvCfg config.Config) (*httptest.Server, *balancer.Pool, *atomic.Int32) {
	t.Helper()

	var complete atomic.Int32
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n, err := io.Copy(io.Discard, r.Body)
		if err == nil && n == 1000 {
			complete.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(be.Close)

	b, err := balancer.NewBackend(be.URL,
		balancer.WithBreakerThreshold(1),
		balancer.WithBreakerCooldown(time.Hour))
	require.NoError(t, err)
	pool := balancer.NewPool([]*balancer.Backend{b})

	m := metrics.New()
	proxied := m.Wrap(proxy.NewServer(pool,
		proxy.WithMetrics(m),
		proxy.WithMaxBodyBytes(1<<20)))
	mux := newMux(pool, newAdminAPI(pool, http.DefaultTransport, m, 3, 5*time.Second), m, proxied)

	ts := httptest.NewUnstartedServer(nil)
	ts.Config = newHTTPServer(srvCfg, mux)
	ts.Start()
	t.Cleanup(ts.Close)

	return ts, pool, &complete
}

// trickleUpload sends the 1000-byte body in 10 chunks, sleeping before each one
// so the *last* byte lands after total — it has to outlast the ReadTimeout the
// server used to hardcode, so the pause comes first, not after. It reports the
// status line, or "" when the server closed the connection without answering.
func trickleUpload(t *testing.T, addr string, total time.Duration) string {
	t.Helper()

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	_, err = conn.Write([]byte("POST /upload HTTP/1.1\r\nHost: lb\r\nContent-Length: 1000\r\n\r\n"))
	require.NoError(t, err)

	const chunk = 100
	pause := total / 10
	for range 10 {
		time.Sleep(pause)
		if _, err := conn.Write(bytes.Repeat([]byte("x"), chunk)); err != nil {
			break
		}
	}

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(10*time.Second)))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.Status
}

// TestServerTimeoutsDefaultToNoDeadline pins the default itself, so the
// behavioural test below does not have to be the only thing standing between a
// regression and a 5s upload limit.
func TestServerTimeoutsDefaultToNoDeadline(t *testing.T) {
	srv := newHTTPServer(config.Config{}, http.NotFoundHandler())
	assert.Zero(t, srv.ReadTimeout, "no whole-request deadline by default")
	assert.Zero(t, srv.WriteTimeout, "no response deadline by default")
	assert.Equal(t, 5*time.Second, srv.ReadHeaderTimeout, "header phase stays bounded")
	assert.Equal(t, 60*time.Second, srv.IdleTimeout, "keep-alive stays bounded")
}

// TestSlowUploadIsNotTruncatedByDefault is the regression test for finding 7.
// The server used to hardcode ReadTimeout: 5s, which bounds the whole request
// including the body, so this client — 1000 bytes over 6s — was cut off
// mid-transfer. The observed result was worse than a truncated request: the body
// read failed inside ReverseProxy, which the proxy charged to the backend as a
// transport error, giving the client a 502 and opening the circuit of a backend
// that was answering fine. The documented limit is -max-body-bytes, not a clock.
//
// Takes 6s on purpose: the last byte has to land after the old hardcoded 5s for
// this test to fail against it.
func TestSlowUploadIsNotTruncatedByDefault(t *testing.T) {
	ts, pool, complete := newSlowUploadFixture(t, config.Config{})

	assert.Equal(t, "200 OK", trickleUpload(t, ts.Listener.Addr().String(), 6*time.Second))
	assert.Equal(t, int32(1), complete.Load(), "the backend must receive the whole body")
	assert.True(t, pool.Ready(), "a slow client must not open the backend's circuit")
}

// TestSlowUploadCutOffByReadTimeoutKeepsTheCircuit is the end-to-end half of
// review finding 9. With -read-timeout set, the server does cut the transfer —
// that is what the flag is for. What it must not do is blame the backend: the
// read fails on the client connection, so the answer is 408 and the backend
// stays in rotation. Before the fix the client got 502 and the backend's circuit
// opened, which at pool scale means /ready reporting 503 and the balancer
// failing its orchestrator probe.
func TestSlowUploadCutOffByReadTimeoutKeepsTheCircuit(t *testing.T) {
	ts, pool, _ := newSlowUploadFixture(t, config.Config{ReadTimeout: time.Second})

	assert.Equal(t, "408 Request Timeout", trickleUpload(t, ts.Listener.Addr().String(), 3*time.Second))
	assert.True(t, pool.Ready(), "a client cut off by -read-timeout must not open the backend's circuit")

	// And the backend still serves: it was never at fault.
	assert.Equal(t, "200 OK", trickleUpload(t, ts.Listener.Addr().String(), 0))
}
