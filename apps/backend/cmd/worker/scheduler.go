package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/scheduler"
)

// Independent process mode: admission does not depend on Temporal availability.
func runScheduler() {
	cfg := config.Load()
	log := observability.SetupLogger("info")
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	database, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		log.Error("scheduler database unavailable", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	schedulerCfg := scheduler.Config(config.Scheduler())
	var guard scheduler.Eligibility
	if schedulerCfg.JiraGuard {
		client, err := jira.NewClient(cfg.JiraBaseURL, cfg.JiraAPIEmail, cfg.JiraAPIToken)
		if err != nil {
			log.Error("scheduler Jira configuration invalid", "error", err)
			os.Exit(1)
		}
		guard = application.SchedulerJiraGuard{Store: persistence.NewPgRequirementStore(database.Pool, database.Queries), Jira: client, EligibleStatus: func(project, status string) bool {
			ids, ok := cfg.StartStatusIDsFor(project)
			if !ok {
				return false
			}
			for _, id := range ids {
				if id == status {
					return true
				}
			}
			return false
		}}
	}
	service := scheduler.New(persistence.NewPgSchedulerStore(database.Pool, database.Queries), schedulerCfg, nil, guard, log)
	handler := func(ctx context.Context, _ []byte) error { return service.Tick(ctx) }
	dispatcher := application.NewOutboxDispatcher(persistence.NewPgOutboxStore(database.Queries), "scheduler", map[string]application.EventHandler{application.OutboxEventTaskRunCreated: handler, application.OutboxEventTaskRunStateChanged: handler}, log, application.OutboxDispatcherConfig{})
	done := make(chan error, 1)
	go func() { done <- dispatcher.Run(ctx) }()
	if err := service.Run(ctx); err != nil {
		log.Error("scheduler stopped", "error", err)
	}
	stop()
	if err := <-done; err != nil {
		log.Error("scheduler dispatcher stopped", "error", err)
	}
}
