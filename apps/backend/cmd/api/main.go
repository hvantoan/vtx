package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/gitops"
	githubintegration "github.com/vtx/platform/internal/integrations/github"
	"github.com/vtx/platform/internal/integrations/jira"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration"
	"github.com/vtx/platform/internal/persistence"
	transporthttp "github.com/vtx/platform/internal/transport/http"
)

func main() {
	cfg := config.Load()
	logger := observability.SetupLogger("info")

	if err := cfg.Validate(); err != nil {
		logger.Error("Invalid configuration", "error", err)
		os.Exit(1)
	}

	// Jira intake config audit: log the resolved policy (project keys and
	// status IDs are not secrets) — never file contents or credentials.
	{
		attrs := []any{
			slog.String("source", cfg.JiraConfigSource()),
			slog.Any("projects", cfg.JiraProjectKeys()),
		}
		var ids []string
		for _, k := range cfg.JiraProjectKeys() {
			if list, ok := cfg.StartStatusIDsFor(k); ok {
				ids = append(ids, list...)
			}
		}
		attrs = append(attrs, slog.Any("status_ids", ids))
		logger.Info("jira intake config", attrs...)
	}

	logger.Info("Starting VTX Platform API",
		slog.String("port", cfg.Port),
		slog.String("env", cfg.Env),
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Connect to Database
	var db *persistence.Database
	var dbDep transporthttp.DependencyChecker
	dbConn, err := persistence.NewDatabase(ctx, cfg)
	if err != nil {
		logger.Warn("Database initial connection warning (will retry on readiness probe)", "error", err)
	} else {
		db = dbConn
		dbDep = db
		defer db.Close()
	}

	// Connect to Temporal
	var temporalDep transporthttp.DependencyChecker
	temporalClient, err := orchestration.NewTemporalClient(cfg.TemporalHostPort, cfg.TemporalNamespace)
	if err != nil {
		logger.Warn("Temporal initial connection warning (will retry on readiness probe)", "error", err)
	} else {
		temporalDep = temporalClient
		defer temporalClient.Close()
	}

	// Jira webhook intake service (VTX-2). The API cannot serve the route
	// correctly without a live DB pool — fail closed at startup rather than
	// claim dedup keys it cannot persist.
	var webhookSvc *application.JiraWebhookService
	var receiptRec *application.ReceiptRecorder
	if db == nil {
		logger.Warn("Jira webhook intake disabled until database connects")
	} else {
		jiraClient, err := jira.NewClient(cfg.JiraBaseURL, cfg.JiraAPIEmail, cfg.JiraAPIToken)
		if err != nil {
			logger.Error("Failed to build Jira client", "error", err)
			os.Exit(1)
		}
		webhookSvc = application.NewJiraWebhookService(cfg, db.Pool, db.Queries, jiraClient, logger)
		receiptRec = application.NewReceiptRecorder(db.Queries)

		// Receipt retention: terminal noise rows age out; accepted/duplicate
		// receipts are kept as the audit record (review P2-1).
		go func() {
			ticker := time.NewTicker(6 * time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					delCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
					n, err := receiptRec.DeleteExpiredReceipts(delCtx, application.ReceiptRetentionDays)
					cancel()
					if err != nil {
						logger.Error("webhook receipt retention failed", "error", err)
					} else if n > 0 {
						logger.Info("webhook receipts pruned", "deleted", n)
					}
				}
			}
		}()
	}

	// Create HTTP Server
	server := transporthttp.NewServer(cfg, logger, dbDep, temporalDep)
	if webhookSvc != nil {
		server.WithJiraWebhook(webhookSvc)
	}
	if receiptRec != nil {
		server.WithJiraReceipts(receiptRec)
	}
	// VTX-3 registry API. Fail closed: no DB ⇒ no registry routes; missing
	// actor tokens ⇒ startup error, never an unauthenticated surface.
	if db != nil {
		if err := cfg.ValidateRegistry(); err != nil {
			logger.Error("Invalid registry configuration", "error", err)
			os.Exit(1)
		}
		registryStore := persistence.NewPgRegistryStore(db.Pool, db.Queries)
		registryService := application.NewRegistryService(registryStore, cfg.RegistryConsumerName, application.SystemClock{}, logger)
		server.WithRegistry(registryStore, registryService)
		server.WithSSEEventReader(persistence.NewPgSSEEventReader(db.Queries))
		server.WithOperator(application.NewOperatorService(
			persistence.NewPgOperatorStore(db.Pool, db.Queries), application.SystemClock{},
			cfg.WorkerHeartbeatInterval, cfg.WorkerStaleFactor,
		))
		if cfg.GitHubToken != "" {
			if err := cfg.ValidateGitHub(); err != nil {
				logger.Error("Invalid GitHub configuration", "error", err)
				os.Exit(1)
			}
			githubClient, err := githubintegration.NewClient(cfg.GitHubAPIBaseURL, cfg.GitHubToken)
			if err != nil {
				logger.Error("Invalid GitHub configuration", "error", err)
				os.Exit(1)
			}
			server.WithPullRequests(application.NewPRService(
				persistence.NewPgPRStore(db.Pool, db.Queries), githubClient, registryService,
				cfg.PRBaseBranch, cfg.GitHubRepoAllowlist,
			))
		}
		// VTX-4 worktree surface. The API process owns HTTP reads/writes only;
		// the periodic reconcile lives in the Temporal worker.
		server.WithWorktree(application.NewWorktreeService(
			persistence.NewPgWorktreeStore(db.Pool, db.Queries),
			gitops.Runner{
				Git: gitops.Git{},
				Layout: gitops.Layout{
					WorktreeRoot:   cfg.WorktreeRoot,
					QuarantineRoot: cfg.WorktreeQuarantineRoot,
					RepoCacheRoot:  cfg.RepoCacheRoot,
				},
			},
			cfg.WorktreeOwner,
			cfg.WorktreeLeaseTTL,
			application.SystemClock{},
			logger,
		))
	}
	addr := fmt.Sprintf("%s:%s", cfg.Host, cfg.Port)

	httpServer := &http.Server{
		Addr:              addr,
		Handler:           server.Routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      0, // Disabled to support persistent SSE streaming connections
		IdleTimeout:       120 * time.Second,
	}

	// Background server run
	go func() {
		logger.Info("HTTP server listening", slog.String("addr", addr))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("HTTP server failed", "error", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	logger.Info("Shutting down server gracefully...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()

	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		logger.Error("Server forced to shutdown", "error", err)
	}

	logger.Info("Server stopped successfully.")
}
