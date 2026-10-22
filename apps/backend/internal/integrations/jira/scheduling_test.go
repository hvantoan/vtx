package jira

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// VTX-5: priority rank and "is blocked by" parsing are admission decisions —
// a misread category or inward/outward direction silently grants a slot to a
// blocked task. These cases pin the contract.
func TestFetchSchedulingIssueParsesBlockersAndPriority(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "10014",
			"key": "VTX-5",
			"fields": {
				"priority": {"name": "High"},
				"issuelinks": [
					{"type": {"name": "Blocks", "inward": "is blocked by", "outward": "blocks"},
					 "inwardIssue": {"key": "VTX-3", "fields": {"status": {"statusCategory": {"key": "done"}}}}},
					{"type": {"name": "Blocks", "inward": "is blocked by", "outward": "blocks"},
					 "inwardIssue": {"key": "VTX-4", "fields": {"status": {"statusCategory": {"key": "new"}}}}},
					{"type": {"name": "Blocks", "inward": "is blocked by", "outward": "blocks"},
					 "outwardIssue": {"key": "VTX-7", "fields": {"status": {"statusCategory": {"key": "new"}}}}},
					{"type": {"name": "Relates", "inward": "relates to", "outward": "relates to"},
					 "inwardIssue": {"key": "VTX-9", "fields": {"status": {"statusCategory": {"key": "new"}}}}}
				]
			}
		}`))
	}))
	defer srv.Close()

	// httptest is http on loopback: the client's internal-target exception
	// covers exactly this case.
	c, err := NewClient(srv.URL, "a@b.c", "tok")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, err := c.FetchSchedulingIssue(context.Background(), "10014")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.PriorityName != "High" || got.PriorityRank != 4 {
		t.Fatalf("priority: got %q/%d", got.PriorityName, got.PriorityRank)
	}
	// Only VTX-4: VTX-3 is done, VTX-7 is the outward direction (this issue
	// blocks it — not a blocker), VTX-9 is a non-Blocks link type.
	if len(got.UnresolvedBlockers) != 1 || got.UnresolvedBlockers[0] != "VTX-4" {
		t.Fatalf("blockers: %v", got.UnresolvedBlockers)
	}
}

func TestFetchSchedulingIssueRetryableOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "a@b.c", "tok")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	if _, err := c.FetchSchedulingIssue(context.Background(), "10014"); !errors.Is(err, ErrRetryable) {
		t.Fatalf("expected ErrRetryable, got %v", err)
	}
}

func TestFetchSchedulingIssueMissingPriorityRanksZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"1","key":"VTX-1","fields":{"priority":null,"issuelinks":[]}}`))
	}))
	defer srv.Close()
	c, err := NewClient(srv.URL, "a@b.c", "tok")
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	got, err := c.FetchSchedulingIssue(context.Background(), "1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if got.PriorityRank != 0 || len(got.UnresolvedBlockers) != 0 {
		t.Fatalf("got %+v", got)
	}
}
