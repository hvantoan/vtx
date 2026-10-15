---
title: "VTX-2: Jira Webhook Intake & Deduplication"
description: "Implement secure, recoverable Jira Cloud webhook intake for 'Ready to Dev' transitions with Nginx Gateway, Jira API re-fetch, and PostgreSQL single-transaction deduplication."
status: completed
priority: P1
effort: "3d"
issue: VTX-2
branch: main
tags: [backend, database, api, security]
created: 2026-10-14
---

# VTX-2: Jira Webhook Intake & Deduplication

## Overview

This plan implements the secure intake and deduplication layer for Jira Cloud webhooks as specified in SPEC-AUTO-01 v1.0 (Amendment 01). It establishes an Nginx Secure Webhook Gateway (envsubst-templated `map` + `rewrite … break` + internal Bearer token), a Go HTTP intake service, Jira API re-fetch verification (≤3s budget, SSRF-guarded), and a PostgreSQL single-transaction persistence layer ensuring exactly one durable execution intent and one outbox event per valid status transition to `Ready to Dev` — status ID verified against the live tenant in Phase 1 (transition `id=2` is a placeholder, not spec truth).

## Goals

| # | Goal | Priority |
|---|------|----------|
| 1 | Secure webhook ingress via Nginx Gateway (URL path secret + internal auth) | P1 |
| 2 | Exactly one `execution_intent` + `outbox_events` per unique transition | P1 |
| 3 | Re-fetch Jira API to verify current status (no stale transitions) | P1 |
| 4 | Full E2E coverage for JT-01..JT-18 with evidence log | P1 |

## Phases

| # | Phase | Status |
|---|-------|--------|
| 1 | [Phase 1: Preparation & Security Config](./phase-01-preparation.md) | Pending |
| 2 | [Phase 2: Contracts, Config & Gateway Setup](./phase-02-contracts-config-gateway-setup.md) | Pending |
| 3 | [Phase 3: Migrations, SQLC & Domain Types](./phase-03-migrations-sqlc-domain-models.md) | Pending |
| 4 | [Phase 4: Jira Client Adapter & Application Service](./phase-04-jira-client-adapter-application-service.md) | Pending |
| 5 | [Phase 5: HTTP Handler, Routes & Unit Tests](./phase-05-http-handler-routes-unit-tests.md) | Pending |
| 6 | [Phase 6: E2E Testing, Runbook & Evidence](./phase-06-e2e-testing-runbook-evidence.md) | Pending |

## Success Criteria

- [ ] `make migrate-up` / `make migrate-down` run cleanly.
- [ ] `docker-compose up` starts Nginx Gateway and Go backend on shared network; backend port 49080 stays published for the dashboard — webhook route protected by dedicated `GATEWAY_INTERNAL_AUTH_TOKEN` (≠ `API_AUTH_TOKEN`, asserted at startup).
- [ ] E2E script `infra/scripts/test-jira-webhook.sh` passes 18/18 cases and writes `tests/evidence/jira-webhook-intake.log`.
- [ ] `make test-boundaries`, `make vet`, `make test`, `make build` pass cleanly.
- [ ] `docs/runbooks/jira-webhook-intake.md` documents ops and outbox contract for VTX-3.

## Red Team Review

### Session — 2026-10-15
**Findings:** 22 (22 accepted, 0 rejected) — 4 reviewers: Security Adversary, Failure Mode Analyst, Assumption Destroyer, Scope & Complexity Critic.
**Severity breakdown:** 9 Critical, 7 High, 6 Medium.

| # | Finding | Severity | Applied To |
|---|---------|----------|------------|
| 1 | Nginx can't read OS env vars — envsubst template + `map` required | Critical | Phase 2 |
| 2 | `proxy_pass` URI inside regex `location` invalid — `rewrite … break` + URI-less `proxy_pass` | Critical | Phase 2 |
| 3 | `failed_retryable` inbox row poisons dedup key — terminal outcomes only claim the key | Critical | Phase 3, 4 |
| 4 | `jira_site_id` no source/NULL kills dedup — `JIRA_SITE_ID` config, `NOT NULL` | Critical | Phase 3, 5 |
| 5 | `items[0].to` positional + non-status events → 422 auto-disable — scan for `field=="status"`, 202-ignored | Critical | Phase 5 |
| 6 | Status ID `2` unverified transition ID — live-tenant verification gate in Phase 1 | Critical | Phase 1, 6 |
| 7 | Removing backend port kills dashboard — keep 49080, dedicated internal token instead | Critical | Phase 2 |
| 8 | Env vars never wired into `Config.Load()`/compose `environment:` | Critical | Phase 1 |
| 9 | Inbox missing `intent_id`/`request_id` columns | Critical | Phase 3 |
| 10 | Webhook route must not reuse public `API_AUTH_TOKEN` | High | Phase 5 |
| 11 | SSRF: unvalidated `issueKey` in Jira API path | High | Phase 4 |
| 12 | `Retry-After` sleep stalls past Jira delivery timeout — ≤3s budget, fail-fast 503 | High | Phase 4 |
| 13 | sqlc `WithTx` choreography unspecified — partial-commit risk | High | Phase 4 |
| 14 | Ingress controls dropped: Content-Type, timeout, rate limit, request_id | High | Phase 5 |
| 15 | JT-03 needs ≥2 backend replicas | High | Phase 6 |
| 16 | OpenAPI missing `400` response | Medium | Phase 2 |
| 17 | Inconsistent secret env names across phases | Medium | Phase 1, 2, 6 |
| 18 | Compose doesn't read repo-root `.env` without `--env-file` | Medium | Phase 1 |
| 19 | Cert script writes private keys into repo tree; scan regex misses PKCS#8 | Medium | Phase 2 |
| 20 | Rotation ordering window loses deliveries — overlap procedure | Medium | Phase 2, 6 |
| 21 | Goose `Down` block not required for 00002 | Medium | Phase 3 |
| 22 | Duplicate domain mirrors of sqlc types — `Envelope` only | Medium | Phase 3 |

### Whole-Plan Consistency Sweep
- Files reread: `plan.md`, `phase-01` through `phase-06`
- Decision deltas checked: 22 — all applied to phase files
- Reconciled stale references: status ID `2` → placeholder/verified-in-Phase-1 (plan.md Overview, Phase 1, Phase 6); `failed_retryable` → non-persisted outcome (Phase 3, 4); `WEBHOOK_PATH_SECRET` → `WEBHOOK_PATH_SECRET_PRIMARY/_SECONDARY` (Phase 1, 2, 6); port removal → published 49080 + internal token (Phase 2, 6); `items[0]` → `field=="status"` scan (Phase 4, 5); domain mirror types → `Envelope` only (Phase 3)
- Unresolved contradictions: 0
