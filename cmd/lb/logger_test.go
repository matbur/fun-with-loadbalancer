package main

import (
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
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