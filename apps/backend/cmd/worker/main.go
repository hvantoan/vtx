// Command worker hosts VTX background processes. Each mode owns one process
// boundary (D8/TR-08):
//
//	worker                 — Temporal worker (needs TEMPORAL_HOST_PORT)
//	worker registry        — registry outbox dispatcher (needs PostgreSQL only)
//
// The registry dispatcher deliberately shares the image but not the lifetime:
// a Temporal outage must never stop registry intake, and the temporable worker
// must not depend on registry tokens.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration"
	"github.com/vtx/platform/internal/persistence"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "registry" {
		runRegistryDispatcher()
		return
	}
	runTemporalWorker()
}

func runTemporalWorker() {
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

// runRegistryDispatcher runs the VTX-3 registry intake consumer: it drains
// `execution_intent.created.v1` from the outbox into task runs. It touches
// PostgreSQL and nothing else — no Temporal client is constructed on this path.
func runRegistryDispatcher() {
	cfg := config.Load()
	logger := observability.SetupLogger("info")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		logger.Error("Failed to connect to database", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	if _, err := database.Ping(ctx); err != nil {
		logger.Error("Database ping failed", "error", err)
		os.Exit(1)
	}

	registry := application.NewRegistryService(
		persistence.NewPgRegistryStore(database.Pool, database.Queries),
		cfg.RegistryConsumerName,
		application.SystemClock{},
		logger,
	)
	handlers := map[string]application.EventHandler{
		application.OutboxEventExecutionIntentCreated: func(ctx context.Context, payload []byte) error {
			_, err := registry.ConsumeIntent(ctx, payload)
			return err
		},
	}
	dispatcher := application.NewOutboxDispatcher(
		persistence.NewPgOutboxStore(database.Queries),
		cfg.RegistryConsumerName,
		handlers,
		logger,
		application.OutboxDispatcherConfig{},
	)

	logger.Info("Starting VTX registry dispatcher", slog.String("consumer", cfg.RegistryConsumerName))
	if err := dispatcher.Run(ctx); err != nil {
		logger.Error("Registry dispatcher stopped with error", "error", err)
		os.Exit(1)
	}
	logger.Info("Registry dispatcher stopped")
}
