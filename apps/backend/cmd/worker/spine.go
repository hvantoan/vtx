package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/execcmd"
	"github.com/vtx/platform/internal/gitops"
	githubintegration "github.com/vtx/platform/internal/integrations/github"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration/spineserver"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/qa"
)

// runSpine hosts the VTX-ORCH spine tool server (Phase 1): a unix-socket
// surface that lets the Pi orchestrator extension drive the EXISTING services.
// Like the scheduler/preparing/registry modes, it touches PostgreSQL only — no
// Temporal client — so orchestration survives a Temporal outage.
func runSpine() {
	cfg := config.Load()
	logger := observability.SetupLogger("info")

	if cfg.SpineAuthToken == "" {
		logger.Error("SPINE_AUTH_TOKEN is required to run the spine tool server")
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	database, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		logger.Error("spine database unavailable", "error", err)
		os.Exit(1)
	}
	defer database.Close()

	qualityGit := gitops.Runner{Git: gitops.Git{}, Layout: gitops.Layout{WorktreeRoot: cfg.WorktreeRoot}}
	guards := persistence.EvidenceGuards{Head: qualityGit, WorktreeRoot: cfg.WorktreeRoot}

	registryStore := persistence.NewPgRegistryStore(database.Pool, database.Queries)
	if err := registryStore.WithTransitionGuards(guards.GuardMap()); err != nil {
		logger.Error("spine transition guards invalid", "error", err)
		os.Exit(1)
	}
	registryService := application.NewRegistryService(registryStore, cfg.RegistryConsumerName, application.SystemClock{}, logger)

	ports := spineserver.Ports{
		Transitions: registryService,
		Gate:        persistence.NewPgGateService(database.Pool, database.Queries, guards), // QA Runner is supplied by the agent runtime (Phase 2+). Until then
		// LaunchSession fails closed — no synthetic verdict exists.
		QA: &qa.Service{
			Store:  persistence.NewPgQAStore(database.Pool, database.Queries),
			Git:    qualityGit,
			Layout: gitops.Layout{WorktreeRoot: cfg.WorktreeRoot},
		},
		Verifier: &application.VerificationService{
			Store:  persistence.NewPgVerificationStore(database.Pool, database.Queries),
			Runner: execcmd.ProcessRunner{},
			Git:    qualityGit,
			Layout: gitops.Layout{WorktreeRoot: cfg.WorktreeRoot},
			Clock:  application.SystemClock{},
		},
		LeaseRenewer: application.NewWorktreeService(
			persistence.NewPgWorktreeStore(database.Pool, database.Queries),
			gitops.Runner{Git: gitops.Git{}, Layout: gitops.Layout{
				WorktreeRoot:   cfg.WorktreeRoot,
				QuarantineRoot: cfg.WorktreeQuarantineRoot,
				RepoCacheRoot:  cfg.RepoCacheRoot,
			}},
			cfg.WorktreeOwner, cfg.WorktreeLeaseTTL, application.SystemClock{}, logger,
		),
	}

	if intake, err := newPollIntake(cfg, database, logger); err != nil {
		logger.Error("spine poll intake configuration invalid", "error", err)
		os.Exit(1)
	} else if intake != nil {
		ports.Intake = intake
	} else {
		logger.Warn("JIRA_* unset; intake tool disabled")
	}

	if cfg.GitHubToken != "" {
		if err := cfg.ValidateGitHub(); err != nil {
			logger.Error("spine GitHub configuration invalid", "error", err)
			os.Exit(1)
		}
		githubClient, err := githubintegration.NewClient(cfg.GitHubAPIBaseURL, cfg.GitHubToken)
		if err != nil {
			logger.Error("spine GitHub client invalid", "error", err)
			os.Exit(1)
		}
		ports.PRs = application.NewPRService(
			persistence.NewPgPRStore(database.Pool, database.Queries), githubClient, registryService,
			cfg.PRBaseBranch, cfg.GitHubRepoAllowlist,
		)
	} else {
		logger.Warn("GITHUB_TOKEN unset; open_pr tool disabled")
	}

	server, err := spineserver.New(spineserver.Config{
		SocketPath: cfg.SpineSocketPath,
		AuthToken:  cfg.SpineAuthToken,
		LeaseOwner: cfg.WorktreeOwner,
	}, ports, logger)
	if err != nil {
		logger.Error("spine server configuration invalid", "error", err)
		os.Exit(1)
	}

	logger.Info("starting VTX spine tool server",
		slog.String("socket", cfg.SpineSocketPath),
		slog.String("lease_owner", cfg.WorktreeOwner),
	)
	if err := server.Serve(ctx); err != nil {
		logger.Error("spine server stopped with error", "error", err)
		os.Exit(1)
	}
	logger.Info("spine server stopped")
}

// newPollIntake builds the poll-intake service. Credentials are optional: with
// none the tool stays unavailable (fail closed at call time, not at startup);
// with credentials but an invalid project/status policy, startup fails so a
// misconfigured spine never accepts work.
func newPollIntake(cfg *config.Config, database *persistence.Database, logger *slog.Logger) (spineserver.Intake, error) {
	if cfg.JiraBaseURL == "" || cfg.JiraAPIEmail == "" || cfg.JiraAPIToken == "" {
		return nil, nil
	}
	if err := cfg.ValidateJiraIntake(); err != nil {
		return nil, err
	}
	client, err := jira.NewClient(cfg.JiraBaseURL, cfg.JiraAPIEmail, cfg.JiraAPIToken)
	if err != nil {
		return nil, err
	}
	return application.NewPollIntakeService(cfg, database.Pool, database.Queries, client, application.SystemClock{}, logger), nil
}
