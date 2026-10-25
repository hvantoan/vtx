# PLAN — VTX-5..13 Implementation (virtual team)

Baseline: `a0d9b4e` on `dev`. Design specs + contracts merged (task `design-specs` ✔).
Goal task tree: vtx5 → e2e-acceptance (8 remaining).

## Canonical migration map (from reconciled specs)

| File | Lane | Content |
|---|---|---|
| 00006_requirements | B1 | VTX-6 requirements/baseline |
| 00007_agent_runtime | R | VTX-7 agent session registry (backend side) |
| 00008_verification | B2 | VTX-8 verification_runs |
| 00009_qa | B2 | VTX-9 QA evidence + verdicts |
| 00010_quality_gate | B2 | VTX-10 gate_decisions |
| 00011_scheduler | B1 | VTX-5 leases/deferred intents |
| 00012_draft_pr | B3 | VTX-12 pull_requests |
| 00013_ops | B2 | VTX-11 retry verbs + outbox DLQ cols |
| 00014_operator | B3 | VTX-13 operator_audit_log (spec said 00013_ops; split to keep single-writer per file) |

## Parallel lanes (one writer per worktree)

| Lane | Scope (dirs) | Epics | Model |
|---|---|---|---|
| B1 | apps/backend only | VTX-5 scheduler + VTX-6 baseline | openai-codex/gpt-6.1-sol |
| B2 | apps/backend only | VTX-8 exec adapters + VTX-9 QA + VTX-10 gate + VTX-11 retry | devin/swe-2 |
| B3 | apps/backend only | VTX-12 draft PR + VTX-13 operator API | devin/gpt-5.6-sol |
| R | apps/agent-runtime + backend migration 00007 | VTX-7 real agent runtime | devin/claude-sonnet-5.5 |
| W | apps/web only | VTX-13 dashboard UI | openai-codex/gpt-5.6-sol |

Rules given to every lane: read `AGENTS.md`, own spec(s) in `docs/design/`, contracts in `contracts/`; write unit tests proving contract items; run scoped `make vet/typecheck/test`; commit conventionally on lane branch; NEVER merge to dev; report deviations + NOT VERIFIED items.

## Merge order (after lanes return)

1. R (runtime + 00007) → B1 → B2 → B3 → W.
2. After each backend merge: re-run `sqlc generate` (do not hand-merge `internal/persistence/db/*`), reconcile `routes.go`, `domain/models.go`, `config`, `cmd/*` wiring.
3. Renumber migrations only if a lane deviated; keep map above.

## Integration & evidence checklist (me)

- [x] PLAN committed (b0b0b2f); compose stack up (postgres:49432, temporal:49233); throwaway DB `vtx_test` created for integration suites (`TEST_DATABASE_URL=postgres://vtx_user:vtx_password@localhost:49432/vtx_test?sslmode=disable`).
- [ ] `make test-boundaries vet typecheck test build` clean at HEAD.
- [ ] `make test-integration TEST_DATABASE_URL=...` for DB-backed evidence.
- [ ] Evidence pack `tests/evidence/` per SRS §9: admission-cap, lease-fencing, QA exact-SHA, gate policy, retry budget, draft-PR-after-PASS, agent smoke.
- [ ] E2E-01..07 per SRS §9.2 — verified or explicit BLOCKED/NOT VERIFIED with reason.
- [ ] Update goal tasks with evidence per contract.

## SRS §4 functional MUSTs to trace (audit at merge)

- FR-SCH-001: admit ≤3 concurrent coding runs; over-cap queued per policy → lane B1
- FR-SCH-002: lease owner + monotonic epoch + expiry + renew; stale fenced from state/side-effects → B1
- FR-SCH-003: per-run worktree create/validate/cleanup; no cross-run reuse → exists (VTX-4), B1 scheduler must honor
- FR-SCH-004: crash recovery keeps ownership; cleanup never removes leased worktree → B1 + existing
- FR-REG-004/008: deferred intent durable; VTX-5 owns promotion/admission + lease grant/renew → B1
- FR-AGT-001: agent gets scope + repo/worktree + test baseline; emits artifacts/SHA/logs → B1(baseline) + R
- FR-AGT-002: handshake/version negotiation, cancel, event stream, process-group cleanup → R
- FR-AGT-003: Pi/OMP simulation labeled test double → R
- FR-QA-001: independent QA ≠ coding agent, evaluates exact SHA + test evidence → B2
- FR-QA-002: gate PASS only when mandatory conditions hold, not LLM-alone → B2
- FR-QA-003: SHA change after QA invalidates prior evidence → B2
- FR-PR-001: Draft PR only after gate PASS; no auto-merge/deploy → B3
- FR-ERR-001: bounded retry, terminal failure + evidence → B2
- FR-UI-001: dashboard shows measured state/readiness not hardcode → W
- FR-UI-003: operator mutations need authenticated principal + authz + audit(actor/reason/correlation) → B3
- FR-UI-004: SSE auth via header/session, never query token → B3 + W
- FR-EVT-002: outbox DLQ + redrive with audit → B3 (exists for dispatcher)
- FR-JIRA-001..007, FR-REG-001..003/005..007/009, FR-EVT-001/003..005: already implemented (VTX-2/3/4) — regression-test only

## E2E (SRS §9.2)

- E2E-01: Ready-to-Dev issue → intent → queued → admission → agent commit → QA PASS → Draft PR
- E2E-02: duplicate webhook ≥3× → exactly one run, no dup PR
- E2E-03: worker death during coding → lease recovery, no stale updates/dup side-effects
- E2E-04: QA fail → no PR; bounded audited retries
- E2E-05: SHA change post-QA → gate invalidates evidence
- E2E-06: ≥2 runnable issues vs 3 slots → admission invariant
- E2E-07: operator cancel/resume + DLQ redrive per permission policy + audit

## Merge-reconcile notes

- **Lease recovery implemented TWICE**: task-1 lane has `ReleaseRunLease(owner+epoch)` inside locked tx before policy block; task-2 lane has `TransitionCommand.RecoverExpiredLease` (system-actor, CAS, clear+epoch-bump). Keep ONE at merge — prefer task-2's explicit command if equivalent, port task-1's block-path caller to it.
- **Guard seam**: task-2 adds tx-bound `TransitionGuard`-style seam in persistence RegistryStore (additive, old ctor kept). Wire task-3's DraftPRGuard (PR only after gate PASS) through it.
- **QA AgentRunner port**: task-2 defines it; concrete adapter from task-4 (agent runtime) wired at merge. QA fails closed without runner.
- **Duplicate spec-copied migrations**: task-1 may have 00008, task-3 may have 00010 — compare vs spec at merge, keep spec-conformant copy.

## Deviations log

- 00013_ops split: VTX-11 retry/DLQ in `00013_ops` (B2); VTX-13 `operator_audit_log` moved to `00014_operator` (B3) to keep single-writer per migration file.
