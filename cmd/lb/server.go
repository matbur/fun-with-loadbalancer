package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
)

// serveUntilShutdown serves srv on ln until ctx is canceled (SIGINT/SIGTERM
// in main), then shuts the server down gracefully and blocks until the
// in-flight requests finished or shutdownTimeout elapsed. It returns nil
// after a graceful shutdown and the serve error when the server failed on
// its own.
//
// Waiting on the drain matters: Shutdown first closes the listeners, which
// makes Serve return http.ErrServerClosed immediately, and only then waits
// for in-flight requests. Without the shutdownDone wait, the caller would
// return right after Serve and the process would exit while requests were
// still being handled.
func serveUntilShutdown(ctx context.Context, ln net.Listener, srv *http.Server, logger *slog.Logger) error {
	shutdownDone := make(chan struct{})
	go func() {
		<-ctx.Done()
		logger.Info("shutting down")

		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("shutdown failed", "error", err)
		}
		close(shutdownDone)
	}()

	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-shutdownDone
	return nil
}
