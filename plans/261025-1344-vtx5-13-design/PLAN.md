# PLAN — VTX-5..13 Design + Implementation via Pi Virtual Team

**Goal:** `mvngsp7t-136v9d` — finish design + implement remaining VTX epics (VTX-5..13) per `docs/VTX_Platform_SRS_v0.1.docx`, using Pi subagents in this session.
**Date:** 2026-10-25 · **Branch:** `dev`

## Reference inputs
- `plans/261025-1344-vtx5-13-design/srs-v0.1.txt` — extracted plain text of the SRS docx (17.8k chars).
- Existing code: `apps/backend/internal/{application,domain,orchestration,transport,persistence,agent}`, `apps/agent-runtime/src`, `apps/web/src`, `contracts/`, `db/migrations|queries`.
- Existing docs: `docs/architecture.md`, `DESIGN.md`, `docs/design/design-guidelines.md`, `docs/runbooks/*`, `docs/research/*`.

## Phase 1 — Design specs (task `design-specs`)
4 parallel `worker` lanes, each in an isolated worktree. Outputs = NEW files only under `docs/design/` + `docs/design/contract-drafts/` (coordinator merges into `contracts/`).

| Lane | Epics | Spec files | Contract draft |
|------|-------|-----------|----------------|
| A | VTX-5 scheduler/admission + VTX-6 requirement baseline | `vtx-5-scheduler.md`, `vtx-6-requirement-baseline.md` | `contract-drafts/vtx5-6-events.json` |
| B | VTX-7 agent runtime + VTX-8 exec adapters | `vtx-7-agent-runtime.md`, `vtx-8-execution-adapters.md` | `contract-drafts/vtx7-8-agent-ipc.md` |
| C | VTX-9/10/11 QA + gate + retry | `vtx-9-independent-qa.md`, `vtx-10-quality-gate.md`, `vtx-11-retry-orchestration.md` | `contract-drafts/vtx9-11-events.json` |
| D | VTX-12 draft PR + VTX-13 dashboard | `vtx-12-draft-pr.md`, `vtx-13-dashboard.md` | `contract-drafts/vtx12-13-openapi.yaml` |

Spec requirements per file: FR/NFR coverage table (SRS ID → decision), data model/DDL deltas, API/event changes, state machine interactions, error/retry semantics, test matrix mapped to SRS §9, remaining [PROPOSED]/OD open decisions.

Merge: copy new files back from each worktree → coordinator merges contract drafts into `contracts/` → fresh `reviewer` child audits coverage vs SRS §4-§8 → fix gaps → complete task.

## Phase 2 — Implementation (tasks vtx5-scheduler … vtx13-dashboard)
Dependency order: vtx5 → vtx6 → vtx7 → vtx8 → vtx9-11 → vtx12 → vtx13 → e2e.
Each implementation lane: `worker` in worktree → coordinator reviews diff → `reviewer` pass → merge + `make test` gate → next lane. Independent lanes may run parallel when they touch disjoint packages.

## Phase 3 — E2E acceptance
Run E2E-01..07 per SRS §9.2; store evidence in `tests/evidence/`; unverifiable items marked NOT VERIFIED with reason.

## Rules
- Workers never commit; coordinator merges. Markdown only under `docs/` or `plans/`.
- Every `make` verify step: `test-boundaries vet typecheck test build` — zero errors.
- Unverifiable acceptance → BLOCKED/NOT VERIFIED, never fake PASS.

## Progress log
- 2026-10-25 13:4x — Plan written; Phase 1 lanes launching.
