---
title: "Phase 5: HTTP Handler, Routes & Unit Tests"
status: todo
---

# Phase 5: HTTP Handler, Routes & Unit Tests

## Overview

Implement `HandleJiraWebhook`: envelope parsing that scans `changelog.items[]` for the `status` field (never `items[0]` positionally), dedicated internal-token auth that is **provably different** from the public dashboard token, and the full ingress-control set the spec mandates. Critically: non-status `issue_updated` events (comment, assignee, label edits — the bulk of Jira traffic) return **202 acknowledged**, not 422 — sustained non-2xx responses cause Jira Cloud to auto-disable the webhook, which is a total intake outage.

## Requirements

- [x] `apps/backend/internal/transport/http/handlers_jira.go` with `HandleJiraWebhook`.
- [x] Dedicated middleware keyed on `cfg.GatewayInternalAuthToken` — **never** `BearerAuthMiddleware(s.cfg.AuthToken)`: `API_AUTH_TOKEN` ships to every browser bundle (`NEXT_PUBLIC_API_AUTH_TOKEN`), so reusing it collapses both layers into one public credential. Startup assert: `GatewayInternalAuthToken != AuthToken` and non-empty.
- [x] Ingress controls: `Authorization` check (constant-time), `Content-Type: application/json` enforcement → 400, `http.MaxBytesReader` 256 KiB → 413, `Content-Type`+JSON parse → 400, request timeout via `context` deadline, `request_id` = `middleware.GetReqID(r.Context())` in every response.
- [x] Payload extraction: `webhookEvent == "jira:issue_updated"` gate; `issue.id`, `issue.key`, `issue.fields.project.key`, `issue.self` host → `jira_site_id` (validated against `JIRA_SITE_ID`); `changelog.id`; scan `changelog.items[]` for `field == "status"` (or `fieldId == "status"`) → `to` = `target_status_id`.
- [x] Route in `routes.go` on a dedicated group — not inside the dashboard-auth `r.Group`.

## Implementation Steps

1. **Handler flow**:
   - 401 missing/invalid internal token (fail closed); 400 wrong `Content-Type` or malformed JSON; 413 oversized.
   - `webhookEvent != jira:issue_updated` OR no `status` item in changelog OR missing `changelog.id` → **202** `{disposition: "ignored_scope"}` (acknowledge, never penalize Jira for routine traffic). Reserve `invalid_transition` → 422 only for structurally accepted-but-unprocessable envelopes per spec; where in doubt prefer 202-ignored to protect webhook health — and write the inbox row for traceability.
   - Service result mapping: `accepted` → 202 `{request_id, disposition, intent_id}`; `duplicate` → 200 `{request_id, disposition, intent_id}`; `ignored_stale`/`ignored_scope` → 202; `ErrRetryable` → 503 `Retry-After`-friendly (no envelope secrets in body).
   - Log via `slog` with `request_id` + outcome only — never the URL path (gateway already stripped it; assert no `/webhooks/` string reaches logs) or payload body.
2. **Route wiring**: `r.With(GatewayAuthMiddleware(cfg.GatewayInternalAuthToken)).Post("/api/v1/integrations/jira/webhooks", s.HandleJiraWebhook)`.
3. **Unit tests** (`handlers_jira_test.go`): missing/wrong token → 401; `API_AUTH_TOKEN` on webhook route → 401 (proves separation); `text/plain` → 400; >256 KiB → 413; valid single-item + multi-item + no-status-item fixtures → correct dispositions; duplicate → 200 same `intent_id`; service-error → 503.

## Todo

- [x] Handler with status-item scan + 202-for-non-status policy
- [x] Dedicated `GatewayAuthMiddleware` + startup token-difference assert
- [x] Content-Type / size / timeout / request_id controls
- [x] Route wiring outside dashboard auth group
- [x] Unit tests incl. token-separation proof

## Success Criteria

- `go test ./apps/backend/internal/transport/http/...` passes, including the `API_AUTH_TOKEN`-rejected-on-webhook-route test.
- Non-status `issue_updated` fixture → 202 (never 422).
- No request path containing `/webhooks/` appears in Go logs; `request_id` present in every response body.
