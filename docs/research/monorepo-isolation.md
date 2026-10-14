# Monorepo Isolation — Go backend vs pnpm JS workspace

**Date:** 2026-10-14 · **Scope:** VTX-14 · `apps/backend` (single `go.mod`, no `go.work`) + `apps/web`, `apps/agent-runtime` (pnpm workspace)

## 1. Decisions

| Decision | Recommendation |
|---|---|
| Dev entrypoint | Single root `Makefile`; `go -C apps/backend` (Go ≥1.20) — no nested Makefiles |
| `go.work` ban | Defense in depth: `.gitignore` + `export GOWORK=off` + CI check |
| Boundary checks | `infra/scripts/check-boundaries.sh` → `make boundary` + always-on CI job |
| CI filtering | One always-running workflow; `dorny/paths-filter` (SHA-pinned); job `if:`; one required `gate` job |
| Rejected | Workflow-level `on.paths`, per-app Makefiles, `go.work`, `nx`/Turborepo, `apps/*` pnpm glob |

Key fact (GitHub docs, verified): a workflow skipped by `on.paths` leaves required checks **Pending → merge blocked**; a job skipped by `if:` reports **Success**. Hence job-level gating + aggregator, never workflow-level `paths:`. `pnpm-workspace.yaml` enumerates apps explicitly — a glob would absorb any future dir containing `package.json`.

## 2. Root Makefile (sole entrypoint)

```make
SHELL         := /usr/bin/env bash
.DEFAULT_GOAL := help
export GOWORK := off                    # workspace mode hard-disabled
BACKEND       := apps/backend
COMPOSE       := docker compose -f infra/compose/docker-compose.yml
.PHONY: help dev infra build test lint boundary contracts doctor clean run-api run-worker

help: ## List targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
	  awk -F':.*?## ' '{printf "  %-12s %s\n", $$1, $$2}'

dev: infra ## Compose stack + api/worker/web for local dev
	$(MAKE) -j2 run-api run-worker & pnpm --filter @vtx/web dev
infra:   ; $(COMPOSE) up -d postgres temporal temporal-ui
build: build-go build-js
build-go: ; go -C $(BACKEND) build ./...
build-js: ; pnpm -r build
test: test-go test-js boundary
test-go:  ; go -C $(BACKEND) test ./...
test-js:  ; pnpm -r test
lint:     ; go -C $(BACKEND) vet ./... && golangci-lint run $(BACKEND)/... && pnpm -r lint
boundary: ; bash infra/scripts/check-boundaries.sh
contracts: ; pnpm exec spectral lint contracts/openapi/*.yaml
doctor: ## Verify toolchain
	@for t in go node pnpm docker sqlc goose golangci-lint; do \
	  command -v $$t >/dev/null || { echo "MISSING: $$t"; exit 1; }; done
clean: ; $(COMPOSE) down -v; rm -rf apps/web/.next apps/*/node_modules
```

Rules: `.PHONY` all targets; `GOWORK=off` neutralizes stray `go.work` in builds; `go -C` avoids `cd` subshell state; JS via `pnpm -r`/`--filter` only. `pnpm-workspace.yaml`: `packages: [apps/web, apps/agent-runtime]`.

## 3. Boundary script — `infra/scripts/check-boundaries.sh`

```bash
#!/usr/bin/env bash
set -uo pipefail; cd "$(git rev-parse --show-toplevel)"; fail=0
err() { echo "BOUNDARY-VIOLATION: $*" >&2; fail=1; }

# R1 (AC1/BS-19): no go.work — committed or not
find . -name 'go.work*' -not -path './node_modules/*' | grep -q . \
  && err "go.work present (GOWORK=off mitigates; file still banned)"

# R2 (AC1): exactly one go.mod, at apps/backend
mods=$(find . -name go.mod -not -path './node_modules/*')
[ "$mods" = "./apps/backend/go.mod" ] || err "unexpected go.mod: ${mods:-none}"

# R3 (AC15/BS-09): web never references backend internals or Docker
grep -rEl --include='*.ts' --include='*.tsx' --include='*.js' \
  'apps/backend|\.\./?backend|/var/run/docker\.sock|DOCKER_HOST' \
  apps/web | grep . && err "apps/web references backend/Docker"

# R4 (AC15): agent-runtime is JS-only (no Go deps, no backend imports)
grep -rEl --include='*.ts' --include='*.js' 'apps/backend|docker\.sock' \
  apps/agent-runtime | grep . && err "agent-runtime leaks"
grep -E '"(file|link|go|golang):' apps/agent-runtime/package.json \
  && err "non-JS dep in agent-runtime"

# R5 (AC5/BS-19): no forbidden Phase-0 scaffolds
for d in apps/desktop apps/mobile; do [ -d "$d" ] && err "forbidden: $d"; done

[ "$fail" -eq 0 ] && echo "boundary check: OK" || exit 1
```

`grep` exiting 1 on no-match is safe — `err` fires only on printed matches. `.gitignore` must list `go.work` so `find` catches untracked IDE-created copies.

## 4. GitHub Actions — path-aware filters

```yaml
name: ci
on: { pull_request: {}, push: { branches: [main] }, merge_group: {} }
concurrency: { group: ci-${{ github.ref }}, cancel-in-progress: true }

jobs:
  changes:
    runs-on: ubuntu-latest
    outputs: { backend: "${{ steps.f.outputs.backend }}", web: "${{ steps.f.outputs.web }}",
      agent: "${{ steps.f.outputs.agent }}", contracts: "${{ steps.f.outputs.contracts }}",
      shared: "${{ steps.f.outputs.shared }}" }
    steps:
      - uses: actions/checkout@v5   # fetch-depth: 0 for push events
      - uses: dorny/paths-filter@de90cc6fb38fc0963ad72b210f1f284cd68cea36 # v3, SHA-pinned
        id: f
        with:
          filters: |
            backend:   ['apps/backend/**']
            web:       ['apps/web/**']
            agent:     ['apps/agent-runtime/**']
            contracts: ['contracts/**']
            shared:    ['Makefile', 'infra/**', '.github/**', 'pnpm-lock.yaml', 'pnpm-workspace.yaml']

  boundary:                        # always runs; cheap guard for everything
    runs-on: ubuntu-latest
    steps: [ { uses: actions/checkout@v5 }, { run: bash infra/scripts/check-boundaries.sh } ]

  go:
    needs: changes
    if: needs.changes.outputs.backend == 'true' || needs.changes.outputs.contracts == 'true' || needs.changes.outputs.shared == 'true'
    runs-on: ubuntu-latest
    steps: [checkout, setup-go, `make build-go test-go lint`]
  web:
    needs: changes
    if: needs.changes.outputs.web == 'true' || needs.changes.outputs.contracts == 'true' || needs.changes.outputs.shared == 'true'
    runs-on: ubuntu-latest
    steps: [checkout, pnpm setup, `pnpm --filter @vtx/web build lint test`]
  agent:
    needs: changes
    if: needs.changes.outputs.agent == 'true' || needs.changes.outputs.contracts == 'true' || needs.changes.outputs.shared == 'true'
    runs-on: ubuntu-latest
    steps: [checkout, pnpm setup, IPC conformance smoke tests]

  gate:                            # THE single required branch-protection check
    needs: [changes, boundary, go, web, agent]
    if: always()
    runs-on: ubuntu-latest
    steps:
      - run: echo '${{ toJSON(needs) }}' | jq -e 'to_entries | map(.value.result) | all(. == "success" or . == "skipped")'
```

- **BS-18 fan-out** is free: `contracts/**` changes flip `contracts=true`, satisfying every consumer job's `if:`. `merge_group` keeps `gate` valid for merge queues. Branch protection requires **`gate`** only — skipped children report Success, nothing stays Pending; a job skipped because `needs` failed may not block, so `gate` uses `if: always()` + explicit result aggregation.

## 5. Risks & trade-offs

- **`dorny/paths-filter`** (v3, MIT) is de-facto standard but third-party — SHA-pin it. Zero-dep alternative: `git diff --name-only $(git merge-base HEAD origin/${{ github.base_ref }}) | grep -E '^apps/backend/'` as a manual outputs step.
- **Skip-semantics drift:** `gate` passes only when every need is success/skipped — stays correct even if GitHub changes skip behavior. **`GOWORK=off` mitigates, doesn't prevent** — a committed `go.work` still confuses IDEs, hence R1 + `.gitignore`.
- **grep checks are lexical, not semantic** — env-var-indirection leaks escape them; that's review's job per AC15. No import-graph tool justified for 3 apps (KISS). No `nx`/Turborepo/Bazel: a second build system that can't model Go natively; revisit only if apps > ~6 or CI > 15 min.

## 6. Sources & open questions

Sources: GitHub Docs, *Troubleshooting required status checks → skipped but required checks* (verified: workflow `paths:` skip = Pending/blocked; job `if:` skip = Success); Go Modules Reference, `GOWORK` (verified); `dorny/paths-filter` v3 README; pnpm docs (`packages:`, `-r`, `--filter`); `docs/brainstorm-contract.md` AC1/AC5/AC15, BS-09/16/18/19.

Open questions: (1) should `gate` also aggregate `compose-smoke` (BS-02) — heavier job, always-run or gated behind `shared||backend`; (2) `golangci-lint` config pin belongs to the Go toolchain ticket.
