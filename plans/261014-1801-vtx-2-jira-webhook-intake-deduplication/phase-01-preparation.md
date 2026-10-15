---
title: "Phase 1: Preparation & Security Config"
status: todo
---

# Phase 1: Preparation & Security Config

## Overview

Set up environment configuration, secrets plumbing, the Go `Config` extension, live-tenant verification of the `Ready to Dev` status ID, and secure test fixtures — before touching the database or HTTP layer. Every env var added here must be reachable by both the Go process (via `config.Load()`) and the gateway container (via compose `environment:`); a var that only exists in `.env.example` is dead on arrival.

## Requirements

- [x] Extend `apps/backend/internal/config/config.go`: add `JiraBaseURL`, `JiraAPIToken`, `JiraProjectAllowlist`, `JiraReadyToDevStatusID`, `JiraSiteID`, `GatewayInternalAuthToken` fields and load them in `Load()` — **fail-closed**: no insecure defaults for tokens; assert `GatewayInternalAuthToken != AuthToken` at startup.
- [x] Add the same six vars to `.env.example` and local `.env`, plus `WEBHOOK_PATH_SECRET_PRIMARY`, `WEBHOOK_PATH_SECRET_SECONDARY`, `VTX_GATEWAY_PORT=49443`.
- [x] Add the new vars to the `backend` service `environment:` block in `infra/compose/docker-compose.yml` and fix env-file loading (`make compose-up` must pass `--env-file` pointing at the repo-root `.env`, since `docker compose -f infra/compose/docker-compose.yml` does not read it by default).
- [x] Create mock Jira webhook payload fixtures in `apps/backend/internal/integrations/jira/testdata/`.
- [x] Update `docs/architecture.md` with the 2-layer auth design.

## Implementation Steps

1. **Environment Variables & Config**: Add `JIRA_BASE_URL`, `JIRA_API_TOKEN`, `JIRA_PROJECT_ALLOWLIST`, `JIRA_READY_TO_DEV_STATUS_ID`, `JIRA_SITE_ID`, `GATEWAY_INTERNAL_AUTH_TOKEN` (backend) and `WEBHOOK_PATH_SECRET_PRIMARY`, `WEBHOOK_PATH_SECRET_SECONDARY`, `VTX_GATEWAY_PORT` (gateway) to `.env.example`, `.env`, `config.go` fields/`Load()`, and the compose `environment:` blocks. `JIRA_SITE_ID` identifies the Atlassian cloud tenant (e.g. the site host or cloud ID) and feeds the `jira_site_id` dedup column — treat it as config, never payload-derived. One naming contract: `WEBHOOK_PATH_SECRET_PRIMARY`/`_SECONDARY` everywhere; grep checks use exactly these names.
2. **Live-tenant status-ID verification gate**: Query `GET /rest/api/3/status` (or `/rest/api/3/project/VTX/statuses`) on `vtxsolution.atlassian.net` with the API token and record the **numeric status ID** for `Ready To Develope`. The transition list shows transition `id=2`, but status IDs on Jira Cloud are typically `10000+` — `2` is a **placeholder**, not spec truth. Configure `JIRA_READY_TO_DEV_STATUS_ID` with the verified value; if the query is unreachable or the status is absent, report BLOCKED with evidence per spec. Fixtures and `.env.example` must reference the verified value (or clearly marked placeholder), never assert `2`.
3. **Test Fixtures**: Create in `internal/integrations/jira/testdata/`:
   - `webhook_valid_transition.json` — `changelog.items` contains `{field:"status", to:"<verified_id>"}` plus a second non-status item (multi-item changelog, order: non-status item first) to prove positional extraction is wrong.
   - `webhook_stale_transition.json`, `webhook_invalid_project.json`, `webhook_missing_changelog.json`, `webhook_no_status_item.json` (changelog exists but no `status` item — must yield 202 ignored, not 422).
   - Fixtures carry `issue.fields.project.key` and `issue.self` (URL containing the site host) so site/project extraction is exercised.
4. **Docs Update**: Document the 2-layer auth (Nginx URL path secret → Go internal Bearer token, distinct from the dashboard `API_AUTH_TOKEN`) in `docs/architecture.md`.

## Todo

- [x] Extend `config.go` + `Load()` with the six backend keys (fail-closed, tokens differ)
- [x] Add all env vars to `.env`, `.env.example`, compose `environment:`, and `--env-file` wiring
- [x] Verify `Ready to Dev` status ID against the live tenant (BLOCKED if unreachable)
- [x] Create mock JSON fixtures (incl. multi-item changelog + no-status-item cases)
- [x] Update architecture docs

## Success Criteria

- `config.Load()` populates all new fields; startup fails closed when `GATEWAY_INTERNAL_AUTH_TOKEN` is unset or equals `API_AUTH_TOKEN`.
- Verified status ID for `Ready To Develope` is recorded in `.env` and the runbook draft; if unverifiable, task is BLOCKED — no implementation proceeds on the `2` placeholder.
- Fixtures include the multi-item changelog case and a no-status-item case.
