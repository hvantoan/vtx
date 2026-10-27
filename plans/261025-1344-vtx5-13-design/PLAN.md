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
- 13:55 — Lanes B/C/D delivered 7 specs + 2 contract drafts; lane-a truncated w/o files.
- 14:0x — Fix-up workflow: lane-d2 truncated again (no openapi draft); lane-a2 stopped (token burn, 0 files). Coordinator wrote vtx-5/6 specs + vtx5-6-events.json + vtx12-13-openapi.yaml directly.
- 14:1x — Merged: events.json (15 types/defs), openapi.yaml (+11 paths, 7 schemas), agent-ipc.json v1.1. Committed `fb738c4`. Reviewer coverage audit dispatched.
- Pending: reviewer gap report → fix gaps → close design-specs → VTX-5 implementation lane.

## Canonical migration map (reconciled 2026-10-25 after reviewer audit)
00006_requirements.sql (VTX-6 run_requirements) · 00007_agent_runtime.sql (VTX-7 agent_sessions/events) · 00008_verification.sql (VTX-8 verification_runs) · 00009_qa.sql (VTX-9 evidence_artifacts/qa_verdicts) · 00010_quality_gate.sql (VTX-10 gate_decisions) · 00011_scheduler.sql (VTX-5 worker_heartbeats) · 00012_draft_pr.sql (VTX-12 pull_requests + task_runs.pr_url) · 00013_ops.sql (VTX-11 delivery_redrives + VTX-13 operator_audit_log)

## Reviewer audit fixes applied
- gate_verdicts → unified on VTX-10 `gate_decisions` (vtx-12/13 + openapi).
- worker_heartbeats DDL unified (composite PK, run_id+queue+active_runs); heartbeat cadence 15s canonical.
- verification_runs: +`kind` (baseline|verification), `commit_sha`→`subject_commit_sha`; vtx-8 emits `evidence_artifacts(test_results)` for gate C3; `run.evidence_invalidated.v1` owner = commit-recording path.
- vtx-9: QAStore.RecordEvidence ingestion path; vtx-11: redrive endpoint unified to /api/v1/operator/outbox/dlq/{id}/redrive; runbook mermaid `advance` fix.
