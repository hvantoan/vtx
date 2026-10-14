package observability

import (
	"log/slog"
	"os"
)

// SetupLogger initializes the standard slog logger with structured JSON output.
func SetupLogger(level string) *slog.Logger {
	var logLevel slog.Level
	switch level {
	case "debug", "DEBUG":
		logLevel = slog.LevelDebug
	case "warn", "WARN":
		logLevel = slog.LevelWarn
	case "error", "ERROR":
		logLevel = slog.LevelError
	default:
		logLevel = slog.LevelInfo
	}

	opts := &slog.HandlerOptions{
		Level: logLevel,
	}

	handler := slog.NewJSONHandler(os.Stdout, opts)
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger
}
