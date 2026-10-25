# Journal: Ship dev → main, v1.1.0 (2026-10-25)

## What shipped

First full platform landing on `main`: 34 commits, 233 files, +37,591/-1,310.

- **Backend** (`apps/backend`, 130 files): chi HTTP API, pgx/sqlc persistence, Temporal
  workflows — scheduler admission (VTX-5/6), verification pipeline + exact-SHA QA +
  quality gate + retry (VTX-8..11), draft PR lifecycle + operator API/SSE (VTX-12/13),
  outbox DLQ redrive, OpenAPI `/internal/v1` quality surface.
- **Agent runtime** (`apps/agent-runtime`, 18 files): Pi/OMP CLI adapters, session
  registry, host process supervisor with `VTX_PI_BIN`/`VTX_OMP_BIN` overrides.
- **Web** (`apps/web`, 22 files): Next.js 15 operator dashboard — runs list/detail,
  settings, SSE client.
- **Infra**: one-command host bootstrap script, per-worktree port allocation `.envrc`,
  migrations 00001–00014.
- **Evidence**: `tests/evidence/vtx-5-13-e2e-matrix.md` + run logs; design docs and
  runbooks under `docs/`.

## Verification

- `make test-boundaries vet typecheck test build` → **EXIT=0** (re-run at ship time
  on the post-merge tree including the bootstrap commit).
- Backend + agent-runtime + web suites all green (agent-runtime 76 passed / 1 skipped;
  web 10/10); Next.js + tsc + Go builds clean.
- Prior-session evidence (same tree): `make test-integration` green on `vtx_test`;
  real Pi smoke PASS.
- Honest gaps (carried from E2E matrix): real Jira ingress, real GitHub PR object,
  live Temporal workflow drive not yet exercised against production services.

## Decisions

- Version: `1.0.0` → `1.1.0` (minor, user-approved) across root + `apps/*` package.json.
- No CHANGELOG file exists — changelog step skipped per ship contract.
- Route collision resolution from post-merge reconciliation kept: operator DLQ redrive
  stays at `/api/v1/operator/outbox/dlq/{id}/redrive`; per-consumer variant at
  `/internal/v1/outbox/dlq/{id}/redrive`.

## Follow-ups

- Merge PR dev → main, then confirm CI (if configured) is green.
- Wire real Jira/GitHub/Temporal credentials for production verification of the three
  gaps above.
