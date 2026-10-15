---
title: "VTX-2 review fixes: identity gates, receipts audit, CI evidence"
date: 2026-10-15
summary: "Closed 5 P1 + 2 P2 re-review findings on the Jira webhook intake; branch hvantoan/vtx-2-review-fixes @65854b6, JT 22/22"
---

# VTX-2 review fixes: identity gates, receipts audit, CI evidence

## What happened
Executed plan 261015-0431 (7 phases) against the re-review BLOCKED verdict (VTX-2 comment 10014) plus Decision 10015.

- P1 target-status gap: changelog `to` ID now gated against per-project `start_status_ids` from `jira.yaml` (JIRA_CONFIG_PATH, fail-closed, dual-control env rejected). Wrong target -> invalid_transition 422, receipt only.
- P1 identity gap: FetchIssue returns id/project/status; authoritative match required before dedup claim; mismatch -> ignored_scope.
- P1 exposed port: docker-compose.prod.yml strips backend ports; gateway-only path, asserted in CI render check.
- P1 outbox contract: added event_id, changelog_id, occurred_at, correlation_id; contracts updated.
- P1 missing CI evidence: new backend-integration job — goose up/down-to-0/up, sqlc drift check, JT-01..21 (20 concurrent x2 replicas), SHA-stamped artifacts.
- P2 audit gap: webhook_receipts append-only table; malformed JSON -> 400 + rejected receipt; unauthed never writes.
- P2 tenant: verified live — statuses 10007="Ready To Develope", admin webhook capability reachable via /rest/webhooks/1.0/webhook (200 []); no subscription registered yet.

## Decision
Kept receipts append-only rather than deduping them (audit completeness over table size). Used changelog_id field name (user-confirmed) instead of spec's transition_id to avoid VTX-3 misreading. Prod boundary via compose overlay rather than touching the dev profile.

## Verification
JT harness 22 PASS / 0 FAIL incl. JT-19..21. make test-boundaries/vet/typecheck/test/build all clean.

## Next steps
CI run on pushed branch; Jira admin must still register the webhook subscription in the Admin UI (cannot be automated); reviewer re-review of 65854b6.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
