---
title: "Phase 3: Migrations, SQLC & Domain Types"
status: todo
---

# Phase 3: Migrations, SQLC & Domain Types

## Overview

Design the PostgreSQL schema for `jira_webhook_inbox`, `execution_intents`, and `outbox_events`, write SQLC queries, and generate the Go querier. Two critical correctness rules: **only terminal outcomes claim the dedup key** (a `failed_retryable` or `invalid_transition` row in the unique index permanently swallows the legitimate retry — the single worst failure mode in this design), and `jira_site_id` must be `NOT NULL` because Postgres treats `NULL != NULL` — a nullable site column silently disables deduplication entirely.

## Requirements

- [x] Create migration `apps/backend/db/migrations/00002_jira_intake_and_outbox.sql` with explicit `-- +goose Up` **and** `-- +goose Down` blocks (mirror `00001` structure; Down drops in FK-safe order).
- [x] `jira_webhook_inbox` columns: `event_id uuid PK`, `jira_site_id varchar NOT NULL` (from `JIRA_SITE_ID` config — Jira payloads carry no site ID), `issue_id`, `issue_key`, `changelog_id`, `target_status_id`, `outcome`, `payload_hash`, `request_id`/`correlation_id`, `intent_id uuid NULL REFERENCES execution_intents`, `error_detail`, `created_at`, `updated_at`.
- [x] Unique index `idx_jira_webhook_inbox_unique` on `(jira_site_id, issue_id, changelog_id, target_status_id)` — **all columns NOT NULL**.
- [x] `execution_intents` (`intent_id` PK, `status` `pending/processing/done/failed`) and `outbox_events` (`outbox_id` PK, `intent_id` FK, `event_type`, `payload jsonb`, `published_at`, `created_at`).
- [x] SQLC queries in `apps/backend/db/queries/jira.sql` + `sqlc generate`.
- [x] Domain types: add **only** `JiraWebhookEnvelope` (the parsed webhook payload) to `internal/domain/models.go` — do NOT mirror sqlc-generated row structs; the service uses `db.Queries` types directly.

## Implementation Steps

1. **Schema** (`00002_jira_intake_and_outbox.sql`, Goose Up+Down):
   - `outcome` CHECK enum: `accepted`, `ignored_stale`, `ignored_scope`, `invalid_transition` — **deliberately no `failed_retryable` stored row**: on retryable failure the handler returns 503 *before* any dedup insert, leaving the event key unclaimed so Jira's redelivery reprocesses cleanly. (If an audit trail for failed attempts is later needed, it goes to a non-unique `jira_webhook_audit` table — out of scope for this migration.)
   - All unique-key columns `NOT NULL`; FK-safe Down order: `outbox_events` → `jira_webhook_inbox` → `execution_intents`.
2. **Queries**:
   - `InsertJiraWebhookInbox ... ON CONFLICT (jira_site_id, issue_id, changelog_id, target_status_id) DO NOTHING RETURNING *` — empty result = conflict = duplicate.
   - `GetJiraWebhookInboxByEventKey` — fetch existing row + `intent_id` for the duplicate response.
   - `InsertExecutionIntent`, `InsertOutboxEvent`, `UpdateJiraWebhookInboxOutcome` (sets `intent_id` + terminal outcome).
3. **Domain types**: `JiraWebhookEnvelope` only (payload parse target, `changelog.items[]` scan semantics live in Phase 5).

## Todo

- [x] Write migration with Up AND Down blocks
- [x] Terminal-outcome-only design (no persisted `failed_retryable` on the dedup key)
- [x] `jira_site_id`, `intent_id`, `request_id` columns — NOT NULL where required
- [x] Write SQLC queries, run `sqlc generate`
- [x] Add `JiraWebhookEnvelope` domain type only

## Success Criteria

- `make migrate-up` and `make migrate-down` run cleanly (Down block exists and works).
- `sqlc generate` produces `jira.sql.go` with the five queries.
- `\d+ jira_webhook_inbox` shows the composite unique index with all-NOT-NULL key columns and `intent_id` FK column.
