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

	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/observability"
	"github.com/vtx/platform/internal/orchestration"
	"github.com/vtx/platform/internal/persistence"
	transporthttp "github.com/vtx/platform/internal/transport/http"
)

func main() {
	cfg := config.Load()
	logger := observability.SetupLogger("info")

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

	// Create HTTP Server
	server := transporthttp.NewServer(cfg, logger, dbDep, temporalDep)
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
