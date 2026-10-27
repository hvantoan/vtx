// Command worker hosts VTX background processes. Each mode owns one process
// boundary (D8/TR-08):
//
//	worker                 — Temporal worker (needs TEMPORAL_HOST_PORT)
//	worker registry        — registry outbox dispatcher (needs PostgreSQL only)
//	worker scheduler       — admission/lease scheduler (needs PostgreSQL only)
//	worker preparing       — preparing-stage polling fallback (PostgreSQL only)
//	worker spine           — VTX-ORCH spine tool server for the Pi orchestrator
//	                         extension (needs PostgreSQL only)
//	worker orchestrate     — VTX-ORCH launcher: claims coding runs and spawns
//	                         headless Pi orchestrators (PostgreSQL + spine socket)
//	worker user set-password — create/reset the operator login (PostgreSQL only)
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
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/retry"
	"time"
)

// reconcileScheduleID is the stable schedule identity: creating it twice is a
// no-op so every worker replica can attempt registration safely.
const reconcileScheduleID = "vtx-worktree-reconcile"

// ensureReconcileSchedule creates the periodic reconcile schedule once. The
// schedule fires WorktreeReconcileWorkflow on the task queue; workflow history
// gives reconcile passes durability and retry for free.
func ensureReconcileSchedule(ctx context.Context, c client.Client, cfg *config.Config, logger *slog.Logger) {
	schedClient := c.ScheduleClient()
	handle := schedClient.GetHandle(ctx, reconcileScheduleID)
	if _, err := handle.Describe(ctx); err == nil {
		return // already exists
	}
	_, err := schedClient.Create(ctx, client.ScheduleOptions{
		ID: reconcileScheduleID,
		Spec: client.ScheduleSpec{
			Intervals: []client.ScheduleIntervalSpec{{Every: cfg.WorktreeReconcileEvery}},
		},
		Action: &client.ScheduleWorkflowAction{
			ID:        "vtx-worktree-reconcile-run",
			Workflow:  orchestration.WorktreeReconcileWorkflow,
			TaskQueue: cfg.TemporalQueue,
		},
	})
	if err != nil {
		logger.Error("worktree reconcile schedule create failed", "error", err)
		return
	}
	logger.Info("worktree reconcile schedule registered",
		slog.String("schedule_id", reconcileScheduleID),
		slog.Duration("interval", cfg.WorktreeReconcileEvery),
	)
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "scheduler" {
		runScheduler()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "preparing" {
		runPreparing()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "registry" {
		runRegistryDispatcher()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "spine" {
		runSpine()
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "orchestrate" {
		runOrchestrator(len(os.Args) > 2 && os.Args[2] == "--once")
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "user" {
		runUserCommand(os.Args[2:])
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

	// VTX-4: worktree lifecycle needs the DB for its durable rows and the git
	// runner for the disk side. The worker connects lazily-but-once here; a
	// DB outage makes activity calls fail, which Temporal retries.
	ctx := context.Background()
	database, dbErr := persistence.NewDatabase(ctx, cfg)
	if dbErr != nil {
		logger.Error("worktree store unavailable; VTX-4 activities disabled", "error", dbErr)
	} else {
		defer database.Close()
		gitRunner := gitops.Runner{
			Git: gitops.Git{},
			Layout: gitops.Layout{
				WorktreeRoot:   cfg.WorktreeRoot,
				QuarantineRoot: cfg.WorktreeQuarantineRoot,
				RepoCacheRoot:  cfg.RepoCacheRoot,
			},
		}
		worktreeSvc := application.NewWorktreeService(
			persistence.NewPgWorktreeStore(database.Pool, database.Queries),
			gitRunner,
			cfg.WorktreeOwner,
			cfg.WorktreeLeaseTTL,
			application.SystemClock{},
			logger,
		)
		acts := orchestration.NewWorktreeActivities(worktreeSvc)
		w.RegisterActivity(acts)
		w.RegisterWorkflow(orchestration.WorktreeReconcileWorkflow)
		if preparation, err := preparationActivities(database, cfg); err != nil {
			logger.Warn("preparation activities unavailable", "error", err)
		} else {
			w.RegisterActivity(preparation)
			w.RegisterWorkflow(orchestration.PreparationWorkflow)
		}

		// Boot sweep (advise decision 4a): crash-orphans and expired leases
		// left by the previous process are reclaimed before new work starts.
		go func() {
			rep, err := worktreeSvc.Reconcile(context.Background())
			if err != nil {
				logger.Error("worktree boot reconcile failed", "error", err)
				return
			}
			logger.Info("worktree boot reconcile",
				"released", rep.Released,
				"quarantined", rep.Quarantined,
				"orphans_quarantined", rep.OrphansQuarantined,
				"errors", len(rep.Errors),
			)
		}()

		// Periodic reconcile via a Temporal schedule (advise decision 4b):
		// lease expiry happens when OTHER workers die, so a boot sweep alone
		// is not enough. The schedule is idempotent — already-exists is fine.
		ensureReconcileSchedule(ctx, c, cfg, logger)
	}

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
	registry.WithRetryPlanner(retry.DefaultPolicy())
	// Recovery is independent of Temporal availability, just like intake.
	retryService := &retry.Service{Store: persistence.NewPgRetryStore(database.Pool, database.Queries), Registry: registry, Logger: logger}
	recoveryDone := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		if err := retryService.Run(ctx, time.Second); err != nil {
			logger.Error("retry recovery stopped", "error", err)
		}
	}()
	defer func() { stop(); <-recoveryDone }()
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
