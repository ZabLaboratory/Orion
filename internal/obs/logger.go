// Package obs centralises observability concerns: structured logging,
// Prometheus metrics, and a panic recovery middleware. Importing it
// from anywhere else in the service is fine; obs imports nothing
// from internal/* so it has no circular risk.
package obs

import (
	"log/slog"
	"os"
	"strings"

	"github.com/ZabLaboratory/Orion/internal/config"
)

// NewLogger builds a slog.Logger from config. JSON in prod (default),
// text in dev. The returned logger is safe to use as the default with
// slog.SetDefault.
func NewLogger(cfg config.Config) *slog.Logger {
	var lvl slog.Level
	switch strings.ToLower(cfg.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "info":
		lvl = slog.LevelInfo
	case "warn", "warning":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{Level: lvl}
	var handler slog.Handler
	if cfg.LogFormat == config.LogFormatText {
		handler = slog.NewTextHandler(os.Stdout, opts)
	} else {
		handler = slog.NewJSONHandler(os.Stdout, opts)
	}
	return slog.New(handler).With("service", "orion")
}
