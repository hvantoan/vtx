---
title: "Phase 2: FetchIssue — authoritative identity re-fetch"
status: done
---

# Phase 2: FetchIssue authoritative re-fetch

## Overview

Closes review P1-2: `FetchIssueStatus` returns only `statusID`, so project
allowlisting runs on the *untrusted webhook payload*. The Jira API call must
return authoritative issue ID, project key, and status ID; the service compares
all three against the webhook identity and config.

Priority: **P1** · Effort: ~2h · Depends on: Phase 1 (consumes `JiraProjects`)

## Requirements

- [ ] `StatusChecker` interface replaced by `IssueFetcher` returning a struct.
- [ ] Re-fetch already requests `fields=status,project` — surface `project.key`
      and the response `id`; verify response `id` equals the requested
      `issueID` (a paranoid identity check that costs one comparison).
- [ ] All failure modes unchanged: transport error, non-200 (incl. 404),
      decode error, missing `status.id` → wrap `ErrRetryable` so handler
      returns 503 and the dedup key stays unclaimed.
- [ ] Missing `project.key` or `id` in a 200 body → `ErrRetryable` too (Jira
      anomaly, safer to redeliver than to trust-or-drop).

## Related Files

- Modify: `apps/backend/internal/integrations/jira/client.go` — `StatusChecker`
  → `IssueFetcher`; `FetchIssueStatus` → `FetchIssue(ctx, issueID) (FetchedIssue, error)`;
  extend `issueResponse` checks (lines 152–165).
- Modify: `apps/backend/internal/application/jira_webhook_service.go` — field
  `jira jira.StatusChecker` → `jira.IssueFetcher`; call site at ~line 115.
- Modify: `apps/backend/internal/application/jira_webhook_service_test.go` and
  `apps/backend/internal/transport/http/handlers_jira_test.go` — mock updates.
- Modify: `apps/backend/cmd/api/main.go` — constructor signature unchanged
  (interface type only).

## Implementation Steps

1. New type in client.go:
   ```go
   type FetchedIssue struct {
       ID         string
       ProjectKey string
       StatusID   string
   }
   type IssueFetcher interface {
       FetchIssue(ctx context.Context, issueID string) (FetchedIssue, error)
   }
   ```
   Keep `StatusChecker` deleted (clean cutover — only one consumer).
2. In `FetchIssue`: keep `ValidateIssueID` guard, 3s `requestBudget`, basic
   auth, `fields=status,project`. After decode require `body.ID == issueID`,
   `body.Fields.Project.Key != ""`, `body.Fields.Status.ID != ""` — else
   `ErrRetryable`.
3. Update `jira_webhook_service.go` call site (validation semantics live in
   Phase 3 — here just thread the struct through).
4. Update mocks: existing test fixtures already encode stale-transition
   scenarios; extend fake client to return `FetchedIssue`.

## Todo

- [ ] Interface + struct + client changes
- [ ] Service call-site update (compiles, behavior unchanged vs Phase 3)
- [ ] Mock/test updates; add test: Jira returns mismatched `id` → retryable

## Success Criteria

- Unit test: fake Jira returning `project.key=OTHER` and `status.id=10007` →
  service rejects in Phase 3 (this phase only proves the field is returned).
- `go build ./...` + existing jira tests green.

## Risks

- Renaming the interface ripples into every fake — mechanical, caught by
  compiler.
- Do not widen `fields=` beyond `status,project` — least data per spec §3.
