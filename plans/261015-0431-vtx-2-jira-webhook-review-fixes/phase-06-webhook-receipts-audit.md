---
title: "Phase 6: webhook_receipts audit table + retention"
status: done
---

# Phase 6: Webhook receipts audit + retention

## Overview

Closes review P2-1 with the user-approved two-table design. Every
*authenticated* request to the webhook route leaves an append-only
`webhook_receipts` row — including malformed JSON, wrong event types, and
events missing identity fields that cannot legally claim the NOT NULL dedup
key in `jira_webhook_inbox`. Unauthenticated requests (401/403 from
`GatewayAuthMiddleware`, before the handler) never write.

Priority: **P1** · Effort: ~3h · Depends on: Phase 4 (`received_at` param)

## Requirements

- [ ] New table `webhook_receipts`: `receipt_id` UUID PK, `received_at`
      timestamptz, `outcome` varchar CHECK (reuse inbox outcome vocabulary +
      `rejected` for transport rejects like bad content-type/malformed JSON/
      oversized), `reason_code` varchar, `request_id`, `payload_hash` char(64),
      `issue_key` nullable, `error_detail` nullable. **No unique index** —
      receipts never dedup.
- [ ] Receipt written exactly once per authenticated request, **after**
      processing settles (outcome known), best-effort: insert failure logs an
      error but must not change the HTTP response and must never roll back the
      acceptance transaction.
- [ ] Transport-level rejects that still write receipts: non-JSON
      content-type, malformed JSON, >256 KiB body — i.e., everything that got
      past `GatewayAuthMiddleware`. Handler currently returns early for these;
      receipts need either a `defer` recording outcome in the handler, or
      moving these rejects into the service. Recommend `defer`-based recorder
      in the handler with an injected `ReceiptRecorder` interface (keeps
      handler thin, keeps DB out of transport concerns… pragmatic call:
      recorder is its own dependency, not the full service).
- [ ] Unauthenticated: nothing (middleware rejects before handler; verified —
      `GatewayAuthMiddleware` runs before `HandleJiraWebhook`).
- [ ] Retention: `DELETE FROM webhook_receipts WHERE outcome IN
      ('ignored_scope','invalid_transition','rejected') AND received_at <
      now() - interval '7 days'` — run via existing runtime mechanism (check
      for a scheduler/cron in the backend; else a simple goroutine ticker in
      `cmd/api` or a documented `psql` cron — pick whichever exists, do not
      add a new dependency for this).
- [ ] `jira_webhook_inbox` unchanged: still NOT NULL identity cols, still only
      terminal outcomes of fully-identified events.

## Related Files

- Create: `apps/backend/db/migrations/00003_webhook_receipts.sql` (goose
  up/down).
- Modify: `apps/backend/db/queries/jira.sql` — `InsertWebhookReceipt :exec`,
  `DeleteExpiredWebhookReceipts :exec`; run `sqlc generate`.
- Modify: `apps/backend/internal/transport/http/handlers_jira.go` — outcome
  recorder `defer`; `ReceiptRecorder` interface.
- Modify: `apps/backend/internal/application/jira_webhook_service.go` — early
  returns set the recorded outcome (return `Result` already carries
  `Disposition`; map rejection paths to `rejected` + reason codes).
- Modify: `apps/backend/cmd/api/main.go` — wire recorder + retention job.

## Implementation Steps

1. Migration + sqlc gen (`go -C apps/backend generate` / repo make target).
2. `ReceiptRecorder` interface:
   `Record(ctx, Receipt{Outcome, ReasonCode, RequestID, PayloadHash, IssueKey, ErrorDetail}) error`.
3. Handler: capture `receivedAt := time.Now().UTC()` at entry; `defer`
   recorder with final outcome — for transport rejects, record `rejected` +
   reason (`bad_content_type`, `malformed_json`, `payload_too_large`); for
   service calls, record `res.Disposition`.
4. Retryable (503) path: record `failed_retryable` receipt — the event may
   return; the receipt proves we saw it (no dedup implications).
5. Retention: find existing cleanup pattern first (`grep -rn "Ticker\|cron"
   apps/backend`), reuse it.
6. Reason codes enumerated as constants in application package.

## Todo

- [ ] Migration + query + sqlc generate
- [ ] Recorder interface + handler defer wiring
- [ ] Service early-return outcomes → recorder
- [ ] Retention job + test (insert old row, run delete, assert gone)

## Success Criteria

- Malformed JSON with valid gateway token → 400 + one `rejected` receipt row
  (`reason_code=malformed_json`).
- Valid auth, non-status `issue_updated` → 202 + `ignored_scope` receipt.
- Same dedup key redelivered → `duplicate` receipt rows accumulate (append-only
  is correct — they prove redelivery count) while inbox stays at 1 row.
- `goose` up/down/re-up clean on a fresh database.

## Risks

- Receipt write latency on every request — single indexed insert, negligible;
  keep it synchronous so failures are visible in logs (don't go async).
- `payload_hash` for oversized bodies: hash only what was read — document the
  truncated-hash semantics in a column comment.
