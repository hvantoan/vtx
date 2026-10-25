package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/gitops"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/requirement"
)

func preparationActivities(database *persistence.Database, cfg *config.Config) (*orchestration.PreparationActivities, error) {
	client, err := jira.NewClient(cfg.JiraBaseURL, cfg.JiraAPIEmail, cfg.JiraAPIToken)
	if err != nil {
		return nil, err
	}
	client.WithAcceptanceCriteriaField(config.JiraACField())
	layout := gitops.Layout{WorktreeRoot: cfg.WorktreeRoot, QuarantineRoot: cfg.WorktreeQuarantineRoot, RepoCacheRoot: cfg.RepoCacheRoot}
	registryStore := persistence.NewPgRegistryStore(database.Pool, database.Queries)
	return &orchestration.PreparationActivities{
		Worktrees:    application.NewWorktreeService(persistence.NewPgWorktreeStore(database.Pool, database.Queries), gitops.Runner{Git: gitops.Git{}, Layout: layout}, cfg.WorktreeOwner, config.Requirements().LeaseTTL, nil, nil),
		Requirements: requirement.New(persistence.NewPgRequirementStore(database.Pool, database.Queries), client, requirement.ProcessRunner{Layout: layout}, requirement.Config(config.Requirements()), nil),
		Registry:     application.NewRegistryService(registryStore, cfg.RegistryConsumerName, nil, nil), Reader: registryStore,
	}, nil
}

// The polling fallback prepares admitted runs even when event delivery or
// Temporal is down. Per-run baseline advisory locks make replicas replay-safe.
func runPreparing() {
	cfg := config.Load()
	log := observability.SetupLogger("info")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	database, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		log.Error("preparing database unavailable", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	activities, err := preparationActivities(database, cfg)
	if err != nil {
		log.Error("preparing configuration invalid", "error", err)
		os.Exit(1)
	}
	ticker := time.NewTicker(config.Scheduler().Tick)
	defer ticker.Stop()
	for {
		runs, err := database.Queries.ListPreparingRuns(ctx, 100)
		if err != nil && ctx.Err() == nil {
			log.Error("list preparing runs failed", "error", err)
		}
		for _, id := range runs {
			runID := id.String()
			if _, err := activities.PrepareRun(ctx, orchestration.AllocateWorktreeInput{RunID: runID}); err != nil && ctx.Err() == nil {
				log.Error("prepare run failed", "run_id", runID, "error", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
