package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestServeUntilShutdownDrainsInFlight documents the graceful-shutdown
// wiring (review finding "Wiring graceful shutdown w cmd/lb/main.go zabija
// drenaż"): canceling ctx while a request is in flight must not let
// serveUntilShutdown return before the client received its response. The old
// main.go wiring — Shutdown in a fire-and-forget goroutine — fails here,
// because Serve returns as soon as Shutdown closes the listeners, long
// before the drain finishes.
func TestServeUntilShutdownDrainsInFlight(t *testing.T) {
	gotReq := make(chan struct{})
	release := make(chan struct{})

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(gotReq)
			<-release
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	serveDone := make(chan error, 1)
	go func() { serveDone <- serveUntilShutdown(ctx, ln, srv, logger) }()

	clientDone := make(chan error, 1)
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
		t.Fatal("server did not receive the request")
	}

	cancel() // the equivalent of SIGINT/SIGTERM in main()

	// The drain must still be in progress: serveUntilShutdown must not
	// return before the in-flight request finishes.
	select {
	case err := <-serveDone:
		t.Fatalf("serveUntilShutdown returned before the in-flight request finished: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	close(release) // let the handler finish

	select {
	case err := <-clientDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("client did not receive its response after the drain")
	}

	// Only now may serveUntilShutdown return.
	select {
	case err := <-serveDone:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("serveUntilShutdown did not return after the drain finished")
	}
}
