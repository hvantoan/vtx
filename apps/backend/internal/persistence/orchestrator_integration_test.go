//go:build integration

package persistence_test

import (
	"context"
	"testing"
	"time"

	"github.com/vtx/platform/internal/persistence"
)

// TestOrchestratorClaimIsExclusiveAndTokenFenced proves the launcher claim
// ledger: only one holder, a fresh claim cannot be stolen, a stale one can be
// taken over, and the crashed holder's token can no longer beat or release.
func TestOrchestratorClaimIsExclusiveAndTokenFenced(t *testing.T) {
	pool, q := schedulerDB(t)
	ctx := context.Background()
	run := seedRun(t, q, 901)
	runID := run.RunID.String()
	store := persistence.NewOrchestratorStore(pool, q)

	now := time.Now()
	staleBefore := now.Add(-time.Minute)

	ok, err := store.Claim(ctx, runID, "token-a", staleBefore)
	if err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	ok, err = store.Claim(ctx, runID, "token-b", staleBefore)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if ok {
		t.Fatal("a fresh claim must not be stolen")
	}

	held, err := store.Beat(ctx, runID, "token-a")
	if err != nil || !held {
		t.Fatalf("owner beat = %v, %v", held, err)
	}
	held, err = store.Beat(ctx, runID, "token-b")
	if err != nil || held {
		t.Fatalf("non-owner beat = %v, %v", held, err)
	}

	// A takeover is only allowed once the previous beat is older than the window.
	ok, err = store.Claim(ctx, runID, "token-b", time.Now().Add(time.Hour))
	if err != nil || !ok {
		t.Fatalf("stale takeover = %v, %v", ok, err)
	}
	if err := store.Release(ctx, runID, "token-a"); err != nil {
		t.Fatalf("stale release: %v", err)
	}
	held, err = store.Beat(ctx, runID, "token-b")
	if err != nil || !held {
		t.Fatalf("takeover must survive the stale holder's release: %v, %v", held, err)
	}
	if err := store.Release(ctx, runID, "token-b"); err != nil {
		t.Fatalf("release: %v", err)
	}
	held, err = store.Beat(ctx, runID, "token-b")
	if err != nil || held {
		t.Fatalf("released claim must not beat: %v, %v", held, err)
	}
}
