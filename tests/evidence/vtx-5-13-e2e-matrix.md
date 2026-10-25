# VTX-5..13 — E2E-01..07 Acceptance Matrix (SRS §9.2)

- **Spec:** `docs/VTX_Platform_SRS_v0.1.docx` §9.2 (E2E-01..07), §9.1 evidence rules
- **Tree:** committed at `4e9d08477b8f7d27406468f75ec6e8e99e2dc088` when this matrix was captured
- **Env:** PostgreSQL 17.11 via Compose (`vtx-postgres-1`, throwaway DB `vtx_test`), pi 1.4.0 on PATH, node + `apps/agent-runtime/dist` built, gh authed (`hvantoan`)
- **Rule:** PASS only when a committed test/script reproduces it locally on real PG/git; everything unproven is NOT VERIFIED with the reason — no self-awarded PASS.

## How to reproduce

```bash
# unit + domain + services (no DB)
make test-boundaries vet typecheck test build

# integration (real Postgres, real migrations, real git worktrees)
TEST_DATABASE_URL="postgres://vtx_user:vtx_password@localhost:49432/vtx_test?sslmode=disable" \
  make test-integration

# real Pi agent smoke (real CLI, real model call — opt-in)
cd apps/agent-runtime && VTX_SMOKE_PI=1 pnpm vitest run tests/pi-smoke.test.ts
```

Note: the host only ships `pg_dump` v16 while the server is v17. The test
harness `pg_dump` step needs a matching binary — used a PATH shim delegating to
`docker exec vtx-postgres-1 pg_dump` (see `/tmp/pgshim/` recipe in journal).
This is environment tooling, not a product gap.

## Matrix

| E2E | Scenario (SRS §9.2) | Evidence | Result |
|---|---|---|---|
| E2E-01 | Valid Ready-to-Dev Jira issue → one intent → queued run → admission → agent commit → independent QA PASS → Draft PR | Chain proven on real PG/git, segmented: (a) intent→queued admit `registry_consume_integration_test.go::TestConsumeIntentAdmitsThenReplays`; (b) admission+lease `scheduler_requirements_integration_test.go::TestAllocateTwoQueuedRunsWithOneFreeSlot`, `TestSCH04GrantRenewAndExpiredTokenFenced`; (c) real agent commit `agent-runtime/tests/pi-smoke.test.ts` — real pi CLI, `artifacts=[commit]`, `test_double=false` (`tests/evidence/pi-smoke.log`, 19.5s); (d) verify→QA→gate→pr_draft `persistence/quality_integration_test.go::TestQualityPipelineRealWorktreeAndConcurrentGuards` (gate_decision_id stamped in transition audit, 3 evidence events in outbox); (e) Draft PR creation `application/pr_service_test.go::TestOpenDraftPRBindsRequestToPassingRunAndSHA`, `TestOpenDraftPRReplayWithPersistedPRCompletesRun`, `TestOpenDraftPRRefusesMissingOrFailingGateBeforeGitHub` | PASS (pipeline) / NOT VERIFIED (real Jira ingress — no Jira tenant in this env; real GitHub PR object — GitHub calls exercised via controlled client in unit tests only) |
| E2E-02 | Same webhook ≥3× → exactly one run, no duplicate PR | `TestConsumeIntentAdmitsThenReplays` (3 consumes → 1 run/1 audit/1 outbound); `TestDispatchReplayAfterEffectCommittedBeforeAck`; harness `test-registry-outbox.sh` TR-03 (`tests/evidence/registry-outbox.log`, 2 replicas, 6 intents → 1 run each); PR side idempotent via replay test `TestOpenDraftPRReplayWithPersistedPRCompletesRun` | PASS |
| E2E-03 | Worker dies during coding; lease/ownership recovered, no stale updates or duplicate side effects | `quality_integration_test.go::TestRetryRecoveryFencesAtomicallyAndRejectsStaleOrLiveLease` (epoch++ fences dead worker in same tx; stale/live lease both rejected); `scheduler_requirements_integration_test.go::TestSCH04GrantRenewAndExpiredTokenFencing`, `TestSCH05ExpiredLeaseBlocksAndHoldsSlot`; lease CAS `TestLeaseFencingAndTerminalRules`; crash cycles `registry-outbox.log` TR-08/TR-20 | PASS |
| E2E-04 | QA detects failure → no Draft PR; retries bounded and audited | Gate fail blocks advance: `quality_integration_test.go` (`ErrEvidenceInvalid` when verdict≠pass/missing evidence); `TestOpenDraftPRRefusesMissingOrFailingGateBeforeGitHub` (PR never opened without passing gate); bounded retry `internal/retry/policy_test.go::TestRetryBackoffBounds`, `TestRetryBudgetExhaustionIsTerminalWithAudit` (terminal failure + audit row); `TestRetryFunctionalFailuresAndPoison` | PASS |
| E2E-05 | Commit SHA changes after QA → gate invalidates prior QA evidence | `quality_integration_test.go::TestGateRejectsSHAMoveAfterRecordedPass` (SHA move after recorded pass → decision invalid); `TestQualityPipelineRealWorktreeAndConcurrentGuards` (stale-sha verdict → `ErrQASHAMismatch`, nothing persisted); `qa/service_test.go::TestValidateSubmissionExactSHAIndependenceAndEvidence` | PASS |
| E2E-06 | ≥2 runnable issues compete for 3 task slots; admission invariant preserved | `scheduler_requirements_integration_test.go::TestSCH02TwoReplicasRespectAdmissionCap` (two scheduler replicas on real PG, cap=3, no over-admission — `pg_advisory_xact_lock` + live-slot count inside tx); `internal/scheduler/scheduler_test.go::TestSCH01AdmissionCapDefersFourthCodingRun`, `TestSCH06DeferredPromotionFIFOAfterSlotFrees` | PASS |
| E2E-07 | Operator cancel/resume + DLQ redrive follow permission policy and audit | `quality_integration_test.go::TestDLQRedriveAtomicAuditAndReplay` (audit row + redrive atomic; replay is 0-row no-op); `handlers_operator_test.go::TestOperatorRedriveRequiresOperatorAndRecordsAttribution`, `TestOperatorReadEndpointsRequireAllowedActor`; actor-from-token `TestRegistryActorComesFromTheToken`, `TestRegistryBodyActorCannotImpersonate`; `operator_audit_log` written in same tx (`persistence/operator.go::RedriveDeadLetter`) | PASS |

## Honest gaps (NOT VERIFIED)

- **Real Jira ingress end-to-end** — no Jira tenant/credentials in this environment. Inbound is proven up to the webhook boundary (`handlers_jira_test.go` dispositions, dedup, identity check; outbox+receipts integration). Marked NOT VERIFIED per DoD, not self-PASSed.
- **Real GitHub draft PR creation** — `PRService` is driven through a controlled GitHub client in unit tests (binds SHA, refuses missing/failing gate, refuses remote-head drift, replay completes run). Opening a real PR on github.com was not performed; `gh` is authed and a controlled smoke is possible on demand.
- **Temporal workflow path** — `internal/orchestration` compiles and unit-tests pass, but the compose Temporal was not driven through a live preparing→coding workflow run in this session.
