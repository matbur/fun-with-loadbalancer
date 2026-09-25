package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
)

func main() {
	port := flag.String("port", "9001", "port to listen on")
	flag.Parse()

	addr := ":" + *port
	slog.Info("backend listening", "addr", addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		fmt.Fprintf(w, "%s%s", *port, r.URL.Path)
	})
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		w.WriteHeader(http.StatusOK)
	})

	if err := http.ListenAndServe(addr, mux); err != nil {
		slog.Error("server error", "error", err)
		os.Exit(1)
	}
}