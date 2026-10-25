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

- [ ] `make test-boundaries vet typecheck test build` clean at HEAD.
- [ ] Postgres up (`make compose-up`) + `make test-integration TEST_DATABASE_URL=...` for DB-backed evidence.
- [ ] Evidence pack `tests/evidence/` per SRS §9: admission-cap, lease-fencing, QA exact-SHA, gate policy, retry budget, draft-PR-after-PASS, agent smoke.
- [ ] E2E-01..07 per SRS §9.2 — verified or explicit BLOCKED/NOT VERIFIED with reason.
- [ ] Update goal tasks with evidence per contract.

## Deviations log

- 00013_ops split: VTX-11 retry/DLQ in `00013_ops` (B2); VTX-13 `operator_audit_log` moved to `00014_operator` (B3) to keep single-writer per migration file.
