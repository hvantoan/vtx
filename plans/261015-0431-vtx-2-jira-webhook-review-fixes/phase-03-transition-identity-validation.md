---
title: "Phase 3: Transition target + identity validation"
status: done
---

# Phase 3: Transition & identity validation

## Overview

Closes review P1-1 and the identity half of P1-2 inside
`ProcessWebhookTransition`. Today any status transition creates an intent when
the issue currently sits in Ready to Dev — a forged transition to "Done" would
qualify. After this phase only transitions whose changelog `to` status ID is in
the project's configured `start_status_ids` may produce an intent, and only
when Jira's authoritative view confirms identity + project + current status.

Priority: **P1** · Effort: ~3h · Depends on: Phases 1–2

## Requirements

- [ ] `targetStatusID` (from `StatusTransitionTo()`) must be in
      `start_status_ids[projectKey]` — checked **before** the duplicate lookup
      and before any DB write that could claim a dedup key. Wrong-target
      events are receipts only (Phase 6), never inbox rows.
- [ ] After `FetchIssue`: authoritative `ID == env.Issue.ID`,
      `ProjectKey == env.Issue.Fields.Project.Key`, `ProjectKey ∈
      JiraProjects`, and `StatusID ∈ start_status_ids[projectKey]` — else
      terminal inbox row `ignored_scope` (identity mismatch) or
      `ignored_stale` (status drift).
- [ ] Payload `projectKey` pre-check (`projectAllowed`) stays as a cheap gate
      but is no longer the authority.
- [ ] Identity mismatch (webhook says VTX-9, Jira says project OTHER) →
      `ignored_scope` + structured warn log; this is the forged-payload case.
- [ ] Outcome enum: add `invalid_transition` usage for allowlisted-project
      events whose transition target is not a start status — decide once:
      wrong-target = `invalid_transition` receipt (no inbox row, since it has
      full identity but no claim should be made). Keep the inbox CHECK list
      unchanged: `accepted, ignored_stale, ignored_scope, invalid_transition`.

## Related Files

- Modify: `apps/backend/internal/application/jira_webhook_service.go`
  (lines 95–126) — replace `projectAllowed()` + `currentStatus ==
  cfg.JiraReadyToDevStatusID` with allowlist lookups:
  `s.cfg.StartStatusIDsFor(projectKey) (ids, ok)` helper on Config.
- Modify: `apps/backend/internal/config/config.go` — add
  `StartStatusIDsFor(project string) ([]string, bool)`.
- Modify: `apps/backend/internal/application/jira_webhook_service_test.go` —
  new cases: wrong-target transition, project mismatch, status drift,
  happy path with allowlist.

## Implementation Steps

1. `Config.StartStatusIDsFor` returns the project's allowlist; missing project
   → `nil, false`.
2. Service reorder:
   ```
   envelope gates (as today)
   → targetStatusID := env.StatusTransitionTo(); if !ok → receipt-only path
   → projectKey := env.Issue.Fields.Project.Key
   → ids, ok := cfg.StartStatusIDsFor(projectKey); if !ok → receipt invalid/scope
   → if !slices.Contains(ids, targetStatusID) → receipt invalid_transition
   → duplicate check (unchanged)
   → FetchIssue → identity checks → current status ∈ ids
   → accepted tx (unchanged shape)
   ```
3. `projectAllowed()` deleted — replaced by `StartStatusIDsFor` (one model).
4. Event key stays `(site, issue_id, changelog_id, target_status_id)` — with
   the gate in place, `target_status_id` is always a configured start ID for
   inbox rows.

## Todo

- [ ] Gate before dedup; identity checks after fetch
- [ ] Tests: forged-target → receipt only, no inbox row, no Jira call needed
      beyond gate; identity mismatch → `ignored_scope`
- [ ] Update JT-05/JT-06/JT-07 fixtures expectations

## Success Criteria

- Forged webhook: real issue in Ready to Dev, changelog `to=10005` (non-start)
  → response 202/422 per disposition, `webhook_receipts` row, **no** inbox row,
  **no** intent.
- Webhook identity mismatch: `project.key=VTX`, Jira re-fetch `project.key=ABC`
  → `ignored_scope` inbox row, no intent.
- Existing JT-01/JT-02/JT-03 still pass unchanged.

## Risks

- Gate-before-dedup means wrong-target events never dedup — intentional:
  receipts are append-only and wrong-target events carry no dedup claim.
- `issue_key` in event key: unchanged, `issue_id` is the identity column.
