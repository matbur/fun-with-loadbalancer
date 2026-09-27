package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewLoggerText(t *testing.T) {
	logger := newLogger("text", "info")
	assert.IsType(t, &slog.TextHandler{}, logger.Handler())
	assert.True(t, logger.Enabled(t.Context(), slog.LevelInfo))
	assert.False(t, logger.Enabled(t.Context(), slog.LevelDebug))
}

func TestNewLoggerJSON(t *testing.T) {
	logger := newLogger("json", "debug")
	assert.IsType(t, &slog.JSONHandler{}, logger.Handler())
	assert.True(t, logger.Enabled(t.Context(), slog.LevelDebug))
}

func TestNewLoggerLevels(t *testing.T) {
	assert.True(t, newLogger("text", "error").Enabled(t.Context(), slog.LevelError))
	assert.False(t, newLogger("text", "error").Enabled(t.Context(), slog.LevelInfo))
	assert.False(t, newLogger("text", "warn").Enabled(t.Context(), slog.LevelDebug))
}

func TestSlogLevelMapping(t *testing.T) {
	assert.Equal(t, slog.LevelDebug, slogLevel("debug"))
	assert.Equal(t, slog.LevelInfo, slogLevel("info"))
	assert.Equal(t, slog.LevelWarn, slogLevel("warn"))
	assert.Equal(t, slog.LevelError, slogLevel("error"))
	assert.Equal(t, slog.LevelInfo, slogLevel("unknown"), "unknown level falls back to info")
}

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
