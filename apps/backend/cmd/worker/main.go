package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration"
)

func main() {
	cfg := config.Load()
	logger := observability.SetupLogger("info")

	logger.Info("Starting VTX Temporal Worker",
		slog.String("host", cfg.TemporalHostPort),
		slog.String("queue", cfg.TemporalQueue),
	)

	c, err := client.Dial(client.Options{
		HostPort:  cfg.TemporalHostPort,
		Namespace: cfg.TemporalNamespace,
	})
	if err != nil {
		logger.Error("Failed to connect to Temporal server", "error", err)
		os.Exit(1)
	}
	defer c.Close()

	w := worker.New(c, cfg.TemporalQueue, worker.Options{})

	// Register workflows and activities
	w.RegisterWorkflow(orchestration.SampleWorkflow)
	w.RegisterActivity(orchestration.SampleActivity)

	logger.Info("Temporal worker registered workflows and activities. Starting...")

	err = w.Start()
	if err != nil {
		logger.Error("Failed to start worker", "error", err)
		os.Exit(1)
	}

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Stopping worker gracefully...")
	w.Stop()
	logger.Info("Worker stopped successfully.")
}
