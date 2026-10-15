# Decision: UI-first Configuration — Target Personas Dev & BA

**Date:** 2026-10-15
**Status:** Accepted
**Epic:** VTX-1 — Autonomous Development Pipeline
**Context:** Recorded after VTX-2 (Jira webhook intake) shipped with env-var-based configuration.

## Decision

The platform's target users are **Dev and BA personas who are strong analysts but not infrastructure/tooling engineers**. Therefore:

**All business-facing configuration must be manageable through the UI, not via environment variables, `.env` files, or shell access.**

## Rationale — customer insight

- Users operate at the *requirement/pipeline* level: project allowlists, status mappings ("which Jira status triggers intake"), webhook registration, retry/schedule policies, preview/QA settings.
- Expecting these users to edit `.env`, restart containers, or rotate secrets via `docker compose` creates an adoption and operations blocker — and invites misconfiguration of a security-sensitive pipeline.
- Env vars remain correct for *deploy-time infrastructure secrets* (DB credentials, `GATEWAY_INTERNAL_AUTH_TOKEN`, API tokens) — those are set once by whoever deploys the platform, never touched by Dev/BA operators.

## Scope split

| Layer | Mechanism | Who manages |
|---|---|---|
| Business config (project allowlist, status ID mappings, pipeline policies, JQL filters, thresholds) | **Admin/Settings UI** backed by a `platform_config` DB table + REST API | Dev/BA operator |
| Deploy-time infra secrets (DB URL, internal tokens, Jira API token) | Environment / secret store | Deployer (one-time) |

## Consequences

1. **VTX-13 (Operations Dashboard)** absorbs a Settings/Configuration surface — config CRUD, validation, masked secret display, audit trail of changes.
2. A `platform_config` (key-value + versioned) table and `GET/PUT /api/v1/config/*` endpoints become required scope; `config.Load()` env values act as *bootstrap defaults*, UI-managed rows override at runtime.
3. VTX-2's env wiring (`JIRA_PROJECT_ALLOWLIST`, `JIRA_READY_TO_DEV_STATUS_ID`, webhook secrets) remains valid for MVP operation but should migrate behind UI management when the dashboard phase lands — status-ID verification and secret rotation should become guided UI flows.
4. Config changes must be auditable (who/when/what) and must not require a container restart where avoidable.
