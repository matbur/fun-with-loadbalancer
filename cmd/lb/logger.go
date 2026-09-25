package main

import (
	"log/slog"
	"os"
)

// newLogger builds the process logger from the validated config values: a
// JSON handler when format is "json", a text handler otherwise, both with
// the configured minimum level. Invalid values are rejected by config.Parse,
// so unknown inputs fall back to their defaults here.
func newLogger(format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: slogLevel(level)}
	if format == "json" {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}

// slogLevel maps the config level name to slog.Level, falling back to info.
func slogLevel(name string) slog.Level {
	switch name {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default: // "info"
		return slog.LevelInfo
	}
}