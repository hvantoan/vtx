---
title: "Phase 6: E2E Testing, Runbook & Evidence"
status: todo
---

# Phase 6: E2E Testing, Runbook & Evidence

## Overview

Execute the full JT-01..JT-18 matrix against the running Compose stack — including the two-replica concurrency case that actually proves cross-process dedup — generate the evidence log, and write the operations runbook. Mock Jira API only for failure injection (429/5xx/stale); every assertion on dedup verifies **database row counts**, not just HTTP responses — the three silent-loss paths (poisoned retry, NULL site, wrong-item extraction) are invisible at the HTTP layer.

## Requirements

- [x] `infra/scripts/test-jira-webhook.sh` covering JT-01..JT-18.
- [x] JT-03 runs against **≥2 backend replicas** (`docker compose up --scale backend=2` — requires removing `container_name` or a compose profile; Nginx upstream rounds to both).
- [x] Retry-path DB assertions: after any 503, `jira_webhook_inbox` row count for that event key = **0** (dedup key unclaimed); after Jira retry → `accepted` + 1 intent.
- [x] Non-status `issue_updated` → 202 asserted (webhook-health policy).
- [x] Secret-leak audit greps `docker compose logs` for `WEBHOOK_PATH_SECRET_PRIMARY`, `WEBHOOK_PATH_SECRET_SECONDARY`, `GATEWAY_INTERNAL_AUTH_TOKEN` values (not names) — zero matches in gateway **and** backend logs; also assert no `/webhooks/` path string in backend access logs.
- [x] `tests/evidence/jira-webhook-intake.log` and `docs/runbooks/jira-webhook-intake.md` (create `docs/runbooks/`).

## Implementation Steps

1. **E2E script**: `curl -k` through `https://localhost:${VTX_GATEWAY_PORT}/webhooks/<secret>/jira`; mock Jira API (wiremock or stub container) for stale-status/429/500 scenarios; `psql` assertions for row counts in all three tables. JT-10 re-entry uses a distinct `changelog.id`. JT-18 exercises primary→secondary rotation with overlap.
2. **Evidence**: run script, capture output, verify zero secret leakage, write `tests/evidence/jira-webhook-intake.log` (18/18 PASS required for handoff; any FAIL → BLOCKED per spec).
3. **Runbook** (`docs/runbooks/jira-webhook-intake.md`): Jira Admin webhook setup steps (`Issue updated`, JQL `project = VTX`), the **verified** status ID recorded from Phase 1 (plus the exact `GET /rest/api/3/status` query to re-verify — never assert `2`), rotation overlap procedure (deploy gateway accepting both secrets → update Jira URL → retire old secret only after deliveries confirmed on new path), `execution_intent.created.v1` outbox contract for VTX-3 consumers, failure/retry semantics (503 ⇒ Jira redelivery ⇒ reprocessing; dedup key never claimed on retryable failure).

## Todo

- [x] `test-jira-webhook.sh` all 18 cases with DB row-count assertions
- [x] JT-03 with `--scale backend=2`
- [x] Zero-leak audit across both container logs
- [x] `tests/evidence/jira-webhook-intake.log` generated, 18/18 PASS
- [x] `docs/runbooks/jira-webhook-intake.md` written (verified status ID, rotation overlap, outbox contract)

## Success Criteria

- Script exits 0 with 18/18 PASS in the evidence log.
- Row counts prove: 1 inbox + 1 intent + 1 outbox per unique transition; 0 rows on retryable failure; `intent_id` identical across duplicate responses.
- Zero secret occurrences and zero `/webhooks/` paths in `docker compose logs`.
- `make test-boundaries`, `make vet`, `make test`, `make build` all green.
