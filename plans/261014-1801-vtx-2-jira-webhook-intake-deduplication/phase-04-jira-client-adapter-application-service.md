---
title: "Phase 4: Jira Client Adapter & Application Service"
status: todo
---

# Phase 4: Jira Client Adapter & Application Service

## Overview

Implement the Jira REST API client (with SSRF guards and a bounded total budget — never sleeping `Retry-After` longer than Jira's delivery timeout tolerates) and the application service running the single `pgx.Tx` transaction. Two correctness anchors: the transaction binds every insert through `qtx := queries.WithTx(tx)` (pool-bound `Queries` auto-commits each statement independently — the classic silent partial-commit), and retryable failures return 503 **without** writing the inbox row, so the dedup key stays unclaimed for Jira's redelivery.

## Requirements

- [x] `apps/backend/internal/integrations/jira/client.go`: `FetchIssueStatus(ctx, issueID string) (statusID string, error)` via `/rest/api/3/issue/{issueId}`.
- [x] SSRF guards: `issue.id` (numeric) used for re-fetch, not `issue.key`; validate `issue.id`/`issue.key` against strict regex (`^\d+$` / `^[A-Z][A-Z0-9]+-\d+$`); `url.PathEscape` on path params; `JIRA_BASE_URL` pinned `https://` + fixed host; `CheckRedirect` rejects cross-host/non-HTTPS redirects; never log secrets or full payloads.
- [x] Total synchronous budget ≤ ~3s: one short retry max; on 429 (regardless of `Retry-After` value — honoring 10–60s sleeps exceeds Jira's webhook delivery window) or 5xx/timeout → return retryable error → handler 503. Jira's own redelivery is the retry mechanism.
- [x] `apps/backend/internal/application/jira_webhook_service.go`: `ProcessWebhookTransition(ctx, env domain.JiraWebhookEnvelope) (Result, error)`.
- [x] Transaction choreography: `tx, _ := pool.Begin(ctx); qtx := queries.WithTx(tx); defer tx.Rollback(ctx)` — **all four statements on `qtx`**.

## Implementation Steps

1. **Jira Client**: `net/http` + `Basic Auth` (email + `JIRA_API_TOKEN`), `context` deadline, response `fields=status,project` only. Status/project identity validated from the API response, never the webhook payload.
2. **Service flow**:
   - Reject before any DB write: missing `changelog.id`, no `status` item in `changelog.items[]`, project not in allowlist → these are terminal non-accepted paths (see Phase 5 for which map to 202-ignored vs 422).
   - `FetchIssueStatus`: on transport/API failure → return `ErrRetryable` (→ 503, **no inbox insert** — dedup key must not be claimed).
   - Current status ≠ configured `Ready to Dev` ID → `ignored_stale`; project disallowed → `ignored_scope`. These write inbox-only rows (terminal outcomes).
   - Accepted path — inside **one** `pgx.Tx` via `qtx`:
     a. `InsertJiraWebhookInbox` (`ON CONFLICT DO NOTHING RETURNING`) — zero rows ⇒ conflict ⇒ query `GetJiraWebhookInboxByEventKey`, return its `intent_id` + `duplicate` outcome, commit nothing else.
     b. `InsertExecutionIntent` (status `pending`).
     c. `InsertOutboxEvent` (`execution_intent.created.v1`, sanitized payload — no credentials, no raw webhook body).
     d. `UpdateJiraWebhookInboxOutcome` → `accepted` + link `intent_id`.
   - `Commit` before returning. On any error inside the tx → `Rollback` (deferred) → `ErrRetryable` → 503.
3. **Uniqueness**: the composite unique index is the only dedup mechanism — no `sync.Mutex`, no SELECT-then-INSERT outside the tx.

## Todo

- [x] Jira client with SSRF guards + ≤3s total budget
- [x] Service with `WithTx` single-transaction choreography
- [x] Retryable-failure path skips inbox insert entirely
- [x] Duplicate path returns existing `intent_id`
- [x] Unit tests: mocked Jira client + `pgxmock`/testcontainer transaction tests (duplicate, rollback, ignored paths)

## Success Criteria

- Concurrent duplicate deliveries → exactly 1 intent + 1 outbox; subsequent calls return 200 with same `intent_id`.
- Simulated Jira 429/500/timeout → 503, **zero** rows in `jira_webhook_inbox` (verified by row count, not just response).
- Simulated DB failure mid-transaction → zero partial rows in all three tables.
- Multi-item changelog fixture processed correctly (status item found regardless of position).
