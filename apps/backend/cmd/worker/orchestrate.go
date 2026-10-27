package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration/launcher"
	"github.com/vtx/platform/internal/persistence"
)

// runOrchestrator hosts the VTX-ORCH launcher: it claims runs that reached
// `coding` with a live lease and starts one headless Pi orchestrator per run.
// Like the spine, it needs PostgreSQL and (for the child) the spine socket —
// no Temporal client and no GitHub/Jira credentials of its own.
//
// once=true ticks once and waits for the children it started, which is what
// the E2E verification harness uses.
func runOrchestrator(once bool) {
	cfg := config.Load()
	logger := observability.SetupLogger("info")

	if cfg.SpineAuthToken == "" {
		logger.Error("SPINE_AUTH_TOKEN is required: the orchestrator child renews its lease through the spine")
		os.Exit(1)
	}
	socketPath, err := filepath.Abs(cfg.SpineSocketPath)
	if err != nil {
		logger.Error("SPINE_SOCKET_PATH is invalid", "error", err)
		os.Exit(1)
	}
	binary, err := filepath.Abs(cfg.OrchestratorPiBinary)
	if err != nil || !isExecutable(binary) {
		logger.Error("orchestrator pi binary is missing or not executable", slog.String("path", binary))
		os.Exit(1)
	}
	extension, err := filepath.Abs(cfg.OrchestratorExtension)
	if err != nil {
		logger.Error("ORCHESTRATOR_EXTENSION is invalid", "error", err)
		os.Exit(1)
	}
	if _, err := os.Stat(extension); err != nil {
		logger.Error("orchestrator extension not found", slog.String("path", extension), "error", err)
		os.Exit(1)
	}
	specDir, err := filepath.Abs(cfg.OrchestratorSpecDir)
	if err != nil {
		logger.Error("ORCHESTRATOR_SPEC_DIR is invalid", "error", err)
		os.Exit(1)
	}
	logDir, err := filepath.Abs(cfg.OrchestratorLogDir)
	if err != nil {
		logger.Error("ORCHESTRATOR_LOG_DIR is invalid", "error", err)
		os.Exit(1)
	}

	owner := cfg.WorktreeOwner
	if owner == "" {
		owner = application.DefaultLeaseOwner
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		logger.Error("orchestrator database unavailable", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	service := launcher.New(
		persistence.NewOrchestratorStore(database.Pool, database.Queries),
		launcher.ExecRunner{
			Binary:     binary,
			Extension:  extension,
			SocketPath: socketPath,
			AuthToken:  cfg.SpineAuthToken,
			LogDir:     logDir,
			Log:        logger,
		},
		launcher.Config{
			Owner:       owner,
			Tick:        config.Scheduler().Tick,
			LeaseRenew:  cfg.OrchestratorLeaseRenew,
			ClaimStale:  cfg.OrchestratorClaimStale,
			MaxParallel: cfg.OrchestratorMaxParallel,
			SpecDir:     specDir,
			LeaseTTL:    cfg.WorktreeLeaseTTL,
			Budget: launcher.Budget{
				MaxTurns:       cfg.OrchestratorMaxTurns,
				MaxTokens:      cfg.OrchestratorMaxTokens,
				WallClockMS:    cfg.OrchestratorWallClock.Milliseconds(),
				MaxDelegations: cfg.OrchestratorMaxDelegations,
			},
		},
		application.SystemClock{},
		logger,
	)

	logger.Info("starting VTX orchestrator launcher",
		slog.String("lease_owner", owner),
		slog.String("spine_socket", socketPath),
		slog.String("spec_dir", specDir),
		slog.Int("max_parallel", cfg.OrchestratorMaxParallel),
	)
	if once {
		if err := service.Tick(ctx); err != nil {
			logger.Error("orchestrator tick failed", "error", err)
			os.Exit(1)
		}
		service.Wait()
		logger.Info("orchestrator once pass complete")
		return
	}
	if err := service.Run(ctx); err != nil {
		logger.Error("orchestrator launcher stopped with error", "error", err)
		os.Exit(1)
	}
	service.Wait()
	logger.Info("orchestrator launcher stopped")
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return false
	}
	return info.Mode().Perm()&0o111 != 0
}
