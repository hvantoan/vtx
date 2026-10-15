---
title: "VTX-2 Jira Webhook Intake — Review Fixes (SPEC-AUTO-01 + Amendment 01 + Decision 10015)"
description: "Close 5 P1 + 2 P2 review findings on VTX-2 (Jira comment 10014) and implement per-project start_status_ids config so the webhook intake passes re-review."
status: done
priority: P1
effort: 14h
issue: VTX-2
branch: hvantoan/vtx-2-review-fixes
tags: [bugfix, backend, database, api, security]
blockedBy: []
blocks: []
created: 2026-10-15
---

# VTX-2 Jira Webhook Intake — Review Fixes

## Overview

Implementation commit `d2326d4` was reviewed at HEAD `c72f331` (Jira VTX-2
comment 10014) and judged **BLOCKED** with five P1 and two P2 findings.
Jira comment 10015 additionally replaces the hard-coded
`JIRA_READY_TO_DEV_STATUS_ID` with a configurable per-project
`start_status_ids` allowlist (status IDs, not display names), delivered via a
YAML config file with fail-closed startup validation.

This plan executes the user-confirmed implementation order:

1. Jira YAML config schema + loader
2. `FetchIssue()` authoritative identity re-fetch
3. Transition target-status and identity validation
4. Typed outbox event contract (`changelog_id`, `occurred_at`)
5. Production gateway boundary (compose overlay)
6. `webhook_receipts` audit table + retention
7. CI evidence + tenant verification

## Source Documents

- Jira VTX-2 description: SPEC-AUTO-01 v1.0 (+ §2A Amendment 01 gateway profile)
- Jira VTX-2 comment 10014: implementation review — verdict BLOCKED
- Jira VTX-2 comment 10015: decision — configurable per-project start status IDs
- Implementation under review: commit `d2326d4`, reviewed at `c72f331`

## Goals

| # | Goal | Priority |
|---|------|----------|
| 1 | All five P1 findings closed with verifiable code/infra changes | P1 |
| 2 | Both P2 findings closed: authenticated-event audit + tenant webhook evidence | P1 |
| 3 | JT-01..18 evidence regenerated against the fix commit SHA, CI green | P1 |
| 4 | No dual control between env and YAML for Jira intake config | P1 |
| 5 | `execution_intent.created.v1` contract uses `changelog_id` naming (spec §6 wording updated to match) | P2 |

## Phases

| # | Phase | Status |
|---|-------|--------|
| 1 | [Jira YAML config](./phase-01-jira-yaml-config.md) | Pending |
| 2 | [FetchIssue authoritative re-fetch](./phase-02-fetch-issue-authoritative.md) | Pending |
| 3 | [Transition & identity validation](./phase-03-transition-identity-validation.md) | Pending |
| 4 | [Typed outbox contract](./phase-04-typed-outbox-contract.md) | Pending |
| 5 | [Prod gateway boundary](./phase-05-prod-gateway-boundary.md) | Pending |
| 6 | [Webhook receipts audit](./phase-06-webhook-receipts-audit.md) | Pending |
| 7 | [CI evidence & tenant verification](./phase-07-ci-evidence-tenant.md) | Pending |

## Key Design Decisions (confirmed with user)

- **Two audit surfaces**: `jira_webhook_inbox` keeps its NOT NULL composite
  dedup key and records only fully-identified events; a new append-only
  `webhook_receipts` table records every *authenticated* request including
  malformed/non-status ones (nullable identity). Unauthenticated requests
  (401/403, rejected by `GatewayAuthMiddleware` before the handler) never write.
- **YAML config**: `go.yaml.in/yaml/v3 v3.0.5` is already an indirect dep —
  promote to direct. When `JIRA_CONFIG_PATH` is set, YAML is the single source
  of truth for `projects.*`; if legacy env keys
  (`JIRA_READY_TO_DEV_STATUS_ID`, `JIRA_PROJECT_ALLOWLIST`) are also set →
  startup error, never silent merge.
- **Event contract naming**: field is `changelog_id` (the Jira changelog.id
  value); the word `transition_id` is removed from the contract to avoid
  confusion with Jira workflow transition IDs consumed by VTX-3.
- **`occurred_at`**: taken from the webhook envelope `timestamp` field
  (already parsed in `domain.JiraWebhookEnvelope`), normalized to UTC; on
  missing/invalid timestamp fall back to `received_at` and stamp reason code
  `occurred_at_source=received_at`.
- **Gateway contract already verified**: `infra/compose/nginx/default.conf.template`
  injects `Authorization: Bearer ${GATEWAY_INTERNAL_AUTH_TOKEN}` (overwrites
  client-sent auth), strips the secret path segment upstream, and masks the
  URI in access logs. Phase 5 therefore only needs the compose overlay —
  nginx is already correct.

## Non-Goals

- No Temporal workflow, agent execution, scheduling, or worktree allocation
  (owned by VTX-3 / VTX-5).
- No Jira plan upgrade or Jira Automation `Send web request` dependency.
- No changes to the public dashboard API surface.
- No inbox dedup semantics change: same `(site, issue_id, changelog_id,
  target_status_id)` tuple → exactly one intent; new changelog_id → new intent.

## Success Criteria

- [ ] Forged webhook with non-allowlisted `target_status_id` while issue sits
      in a start status → `invalid_transition`/`ignored_scope`, **zero** intents
- [ ] `grep -r JIRA_READY_TO_DEV_STATUS_ID apps/backend` → no results
- [ ] `docker compose -f docker-compose.yml -f docker-compose.prod.yml config`
      shows no `ports` on `backend`
- [ ] Outbox payload validates against the typed `execution_intent.created.v1`
      schema (event_id, intent_id, issue_id, issue_key, jira_site_id,
      changelog_id, target_status_id, occurred_at, correlation_id)
- [ ] Every authenticated request leaves a `webhook_receipts` row; 20 concurrent
      duplicate deliveries still yield exactly 1 intent + 1 outbox
- [ ] CI runs migrations up/down/re-up and the JT harness with ≥20 concurrent
      deliveries across 2 instances; JT-15..18 run against real nginx
- [ ] Tenant evidence: Admin Webhook menu/subscription/payload verified on the
      VTX site, or VTX-2 is reported BLOCKED with that evidence gap stated

## Risks

| Risk | Mitigation |
|------|-----------|
| VTX-3 may already consume `transition_id` | Phase 4 greps consumers; stop and ask if a built consumer depends on the old field name |
| Tenant Jira Free may lack Admin Webhooks | Phase 7 verifies early in execution order-independent — if absent, report BLOCKED per spec §2A instead of shipping anonymous intake |
| Receipts table growth from non-status noise | Retention delete for terminal `ignored_*`/`invalid_transition` receipts (7 days); inbox rows retained as dedup records |
| `chimw.Timeout(10s)` vs Jira 3s request budget | Already compatible (3s budget < 10s timeout) — no change |

<!-- slug: vtx-2-jira-webhook-review-fixes -->
