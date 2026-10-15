---
title: "Phase 7: CI evidence & tenant verification"
status: done
---

# Phase 7: CI evidence & tenant verification

## Overview

Closes review P1-5 and P2-2. CI must actually run the things the DoD claims:
goose migrations up/down/re-up, sqlc regeneration diff, and the JT-01..18
harness at AC-02 scale (≥20 concurrent deliveries across two API instances)
including JT-15..18 against real nginx. Tenant evidence confirms the Jira Free
Admin Webhook subscription exists and delivers the payload shape assumed.

Priority: **P1** · Effort: ~3h (repo work) + external tenant task ·
Depends on: Phases 1–6

## Requirements

- [ ] CI job (`.github/workflows/ci.yml` or new `integration.yml`): Postgres 17
      service container → `goose` up → `sqlc generate` → `git diff --exit-code`
      on `internal/persistence/db` → up/down/re-up cycle on clean DB.
- [ ] JT harness in CI: run backend twice (two ports, same DB) + nginx gateway
      container; execute JT-01..18 with **≥20 concurrent** duplicate deliveries
      (current harness uses 8 — fix), asserting exactly 1 intent + 1 outbox.
- [ ] JT-15..18 executed against real nginx in CI (correct secret path →
      forwarded; wrong/missing → 403; forged `Authorization` header stripped;
      secret masked in access log; rotation window both-valid then old-only).
- [ ] Evidence artifact per run: junit/log bundle + rendered `docker compose
  config` for the prod profile + commit SHA; uploaded as workflow artifact.
- [ ] **Tenant verification (external, needs Jira admin — user action)**:
      confirm Administration → Webhooks exists on the Jira Free VTX site,
      create the `Issue updated` subscription scoped to project VTX with
      changelog, fire one real transition to Ready to Dev, capture the payload
      and attach to VTX-2. If the capability is absent → report BLOCKED on
      VTX-2 with evidence (spec §2A mandates this, do not ship anonymous
      intake).
- [ ] Verified tenant facts to encode: `JIRA_SITE_ID`, project key `VTX`,
      `start_status_ids: ["10007"]` (Ready To Develope).
- [ ] Runbook section: env→YAML migration steps for production, gateway URL
      format `https://<gateway>/webhooks/<secret>/jira`, rotation procedure,
      redaction guarantees.

## Related Files

- Modify: `.github/workflows/ci.yml` (or add `integration.yml` — keep one
  workflow file if repo convention prefers; inspect existing jobs first).
- Locate + modify: existing JT harness (search `scripts/`, `apps/backend` test
  files, `Makefile` targets for the 8-concurrent implementation).
- Create: evidence artifact directory convention (e.g. CI artifact upload, not
  committed) — review comment 10014 flagged committed stale evidence; prefer
  artifacts over checked-in files.
- Modify: runbook/docs location used for the Jira webhook config doc.

## Implementation Steps

1. Inspect existing CI + harness; bump concurrency to 20 and spawn a second
   backend process (two `PORT` values, same `DATABASE_URL`) inside the job, or
   docker-compose the pair — reuse Phase 5 compose (base profile) since dev
   ports are published there.
2. Migration job: `goose -dir apps/backend/db/migrations postgres $DSN up`,
   `down-to 0`, `up` again; then `sqlc generate` + `git diff --exit-code`.
3. Gateway tests: run `nginx:stable-alpine` container with the template +
   generated certs; curl cases per JT-15..18.
4. Collect evidence → `actions/upload-artifact` with SHA in the artifact name.
5. Update VTX-2: post the evidence + re-review request comment listing each
   finding → fix commit.

## Todo

- [ ] CI migration/sqlc job
- [ ] JT harness ≥20 concurrent × 2 instances
- [ ] JT-15..18 vs real nginx
- [ ] Evidence artifacts + VTX-2 comment
- [ ] Tenant webhook verified (user/Jira-admin dependent) or BLOCKED reported
- [ ] Runbook: env→YAML migration + gateway ops

## Success Criteria

- CI run on the fix branch shows all jobs green; artifact contains JT-01..18
  results against the branch SHA.
- `docker compose -f base -f prod config` rendering is in the evidence bundle.
- VTX-2 has an evidence comment; every P1/P2 finding maps to a commit.
- If tenant verification fails → issue reported BLOCKED with the exact missing
  capability; no silent PASS.

## Risks

- JT-15..18 need TLS certs in CI — reuse the existing cert generation already
  used for `infra/compose/nginx/certs` (check how dev certs are produced;
  self-signed is fine, gateway tests curl with `-k` or a CA bundle).
- GitHub-hosted runners cap job time — keep the harness runtime < ~10 min;
  compose-based spin-up is the slowest part, cache the image pulls if needed.
