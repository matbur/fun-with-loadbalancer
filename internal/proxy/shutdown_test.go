package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"fun-with-loadbalancer/internal/balancer"
)

func TestGracefulShutdownWaitsForInFlightRequest(t *testing.T) {
	gotReq := make(chan struct{})
	release := make(chan struct{})

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(gotReq)
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	b, err := balancer.NewBackend(backend.URL)
	require.NoError(t, err)

	httpServer := &http.Server{
		Handler:           NewServer(balancer.NewPool([]*balancer.Backend{b})),
		ReadTimeout:       5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	serveDone := make(chan error)
	go func() { serveDone <- httpServer.Serve(ln) }()

	clientDone := make(chan error)
	go func() {
		resp, err := http.Get("http://" + ln.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
		clientDone <- err
	}()

	select {
	case <-gotReq:
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not receive request")
	}

	shutdownDone := make(chan error)
	go func() { shutdownDone <- httpServer.Shutdown(context.Background()) }()

	select {
	case err := <-shutdownDone:
		t.Fatalf("Shutdown returned before in-flight request finished: %v", err)
	default:
	}

	close(release)

	assert.NoError(t, <-shutdownDone)
	assert.NoError(t, <-clientDone)

	select {
	case err := <-serveDone:
		assert.Equal(t, http.ErrServerClosed, err)
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not return after Shutdown")
	}
}