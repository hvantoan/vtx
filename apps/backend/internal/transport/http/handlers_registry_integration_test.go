//go:build integration

// TR-14/TR-23 end to end: real Postgres, real router, real actor tokens.
// Proves an authenticated mutation reaches the registry, that pagination is
// stable on the audit version, and that the API rejects anonymous callers.
package http_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/vtx/platform/internal/application"
	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
	"github.com/vtx/platform/internal/persistence"
	"github.com/vtx/platform/internal/persistence/db"
	transporthttp "github.com/vtx/platform/internal/transport/http"
)

var integrationTokens = map[domain.Actor]string{
	domain.ActorScheduler:    "it-scheduler-token",
	domain.ActorOrchestrator: "it-orchestrator-token",
	domain.ActorPolicy:       "it-policy-token",
}

// TestRegistryAPIAgainstPostgres drives the HTTP surface over a real run.
func TestRegistryAPIAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	queries := db.New(pool)
	store := persistence.NewPgRegistryStore(pool, queries)
	now := time.Date(2026, 10, 19, 6, 30, 0, 0, time.UTC)
	svc := application.NewRegistryService(store, "registry-intake", fixedClock{t: now},
		slog.New(slog.NewTextHandler(io.Discard, nil)))

	cfg := &config.Config{
		GatewayInternalAuthToken: "it-gateway-token",
		RegistryActorTokens:      integrationTokens,
	}
	if err := cfg.ValidateRegistry(); err != nil {
		t.Fatalf("registry config rejected: %v", err)
	}
	server := transporthttp.NewServer(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	server.WithRegistry(store, svc)
	router := server.Routes()

	// Seed one intent + outbox event the way VTX-2 does, then consume it.
	issueID := fmt.Sprintf("8%06d", time.Now().UnixNano()%1000000)
	intent, err := queries.InsertExecutionIntent(ctx, db.InsertExecutionIntentParams{
		Source: "jira_webhook", IssueKey: "VTX-" + issueID, TargetStatusID: "10007",
	})
	if err != nil {
		t.Fatalf("seed intent: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM task_runs WHERE task_id IN (SELECT id FROM registry_tasks WHERE jira_issue_id=$1)`, issueID)
		_, _ = pool.Exec(context.Background(), `DELETE FROM registry_tasks WHERE jira_issue_id=$1`, issueID)
	})

	var intentID, eventID string
	if err := pool.QueryRow(ctx, `SELECT $1::uuid::text, gen_random_uuid()::text`, intent.IntentID).Scan(&intentID, &eventID); err != nil {
		t.Fatalf("render ids: %v", err)
	}
	payload, err := json.Marshal(map[string]string{
		"event_id": eventID, "intent_id": intentID, "issue_id": issueID, "issue_key": "VTX-" + issueID,
		"jira_site_id": "vtxsolution.atlassian.net", "changelog_id": "88888",
		"target_status_id": "10007", "correlation_id": "corr-it",
		"occurred_at": now.Format(time.RFC3339),
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	outcome, err := svc.ConsumeIntent(ctx, payload)
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	runID := outcome.RunID

	do := func(method, path, token string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var reader io.Reader
		if body != nil {
			encoded, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal body: %v", err)
			}
			reader = bytes.NewReader(encoded)
		}
		req := httptest.NewRequest(method, path, reader)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Anonymous mutation is rejected and writes nothing (TR-14).
	if rec := do(http.MethodPost, "/api/v1/registry/runs/"+runID+"/transitions", "",
		map[string]any{"expected_version": 0, "action": "cancel"}); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous transition = %d, want 401", rec.Code)
	}

	// A valid token whose actor may not perform the action is 403: cancel is
	// reserved for operator/policy, never the scheduler.
	if rec := do(http.MethodPost, "/api/v1/registry/runs/"+runID+"/transitions", integrationTokens[domain.ActorScheduler],
		map[string]any{"expected_version": 0, "action": "cancel"}); rec.Code != http.StatusForbidden {
		t.Fatalf("scheduler cancel = %d, want 403", rec.Code)
	}

	// A policy token performs a real mutation (the operator actor now
	// authenticates through an app session; covered by the auth tests).
	rec := do(http.MethodPost, "/api/v1/registry/runs/"+runID+"/transitions", integrationTokens[domain.ActorPolicy],
		map[string]any{"expected_version": 0, "action": "cancel", "reason": "integration",
			"metadata": map[string]any{"source": "integration"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("cancel = %d, want 200 (body %s)", rec.Code, rec.Body)
	}

	// Reading the audit trail is authenticated and version-ordered.
	rec = do(http.MethodGet, "/api/v1/registry/runs/"+runID+"/events?limit=10", integrationTokens[domain.ActorPolicy], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("events = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
	var page struct {
		Items []struct {
			Version int64 `json:"version"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode events: %v", err)
	}
	if len(page.Items) != 2 || page.Items[0].Version != 0 || page.Items[1].Version != 1 {
		t.Fatalf("audit versions = %+v, want [0 1]", page.Items)
	}

	// Cursor pagination returns only rows after the cursor.
	rec = do(http.MethodGet, "/api/v1/registry/runs/"+runID+"/events?after_version=0", integrationTokens[domain.ActorPolicy], nil)
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode paged events: %v", err)
	}
	if len(page.Items) != 1 || page.Items[0].Version != 1 {
		t.Fatalf("after_version=0 returned %+v, want only version 1", page.Items)
	}

	// The task read resolves by immutable Jira identity.
	rec = do(http.MethodGet, "/api/v1/registry/tasks/vtxsolution.atlassian.net/"+issueID,
		integrationTokens[domain.ActorPolicy], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get task = %d, want 200 (body %s)", rec.Code, rec.Body)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }
