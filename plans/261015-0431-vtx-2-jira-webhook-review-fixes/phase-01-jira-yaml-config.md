---
title: "Phase 1: Jira YAML config (per-project start_status_ids)"
status: done
---

# Phase 1: Jira YAML config

## Overview

Implements Jira comment 10015: replace the single `JIRA_READY_TO_DEV_STATUS_ID`
env with a versioned YAML file carrying per-project `start_status_ids`.
Foundation for Phases 2–3 — the validation gates read this config.

Priority: **P1** · Effort: ~3h

## Requirements

- [ ] YAML is loaded only when `JIRA_CONFIG_PATH` is set; the file is the single
      source of truth for project→status mappings.
- [ ] If `JIRA_CONFIG_PATH` is set AND either `JIRA_READY_TO_DEV_STATUS_ID` or
      `JIRA_PROJECT_ALLOWLIST` is also set → startup error (no dual control,
      no silent merge).
- [ ] Legacy env-only deployments still work through a documented migration
      path (env keys valid when `JIRA_CONFIG_PATH` is unset; translated into
      the same `JiraProjects` shape so downstream code sees one model).
- [ ] Startup validation, fail-closed: file missing/unparseable, empty
      `projects`, empty/duplicate/non-numeric `start_status_ids`, unknown top
      keys (strict `KnownFields`) → fatal.
- [ ] Startup `slog.Info` logs loaded project keys and status IDs (IDs are not
      secrets); never log file contents or credentials.
- [ ] `go.yaml.in/yaml/v3` promoted from indirect to direct dependency.

## Schema

```yaml
# config/jira.yaml
jira:
  site_id: "vtxsolution.atlassian.net"   # keeps JIRA_SITE_ID value co-located (optional; env still valid)
  projects:
    VTX:
      start_status_ids: ["10007"]
```

Decide and document: `site_id` stays an env var (recommended — it's
deployment-scoped, not workflow-scoped) or moves into YAML. If moved, apply the
same dual-control error rule.

## Related Files

- Modify: `apps/backend/internal/config/config.go` — add `JiraConfigPath`,
  `JiraProjects map[string]JiraProjectConfig`; remove `JiraReadyToDevStatusID`;
  keep `JiraProjectAllowlist` only for the env-fallback path (or derive it from
  `JiraProjects` keys — prefer deriving, single model).
- Create: `apps/backend/config/jira.yaml` (documented example, not a secret) —
  or keep examples under `docs/`; final location decided at implementation.
- Create: `apps/backend/internal/config/jira_config.go` — loader + validation.
- Modify: `apps/backend/go.mod` — promote yaml.v3 to direct.
- Modify: `infra/compose/docker-compose.yml` — mount `./config/jira.yaml` into
  backend when `JIRA_CONFIG_PATH` is set (dev default can point at the
  checked-in example).
- Modify: `apps/backend/cmd/api/main.go` — startup audit log of loaded config.

## Implementation Steps

1. Define types in `jira_config.go`:
   ```go
   type JiraFileConfig struct {
       Jira struct {
           SiteID   string                        `yaml:"site_id"`
           Projects map[string]JiraProjectConfig  `yaml:"projects"`
       } `yaml:"jira"`
   }
   type JiraProjectConfig struct {
       StartStatusIDs []string `yaml:"start_status_ids"`
   }
   ```
   Decode with `yaml.NewDecoder` + `KnownFields(true)`.
2. `Load()`: read env as today (except `JiraReadyToDevStatusID`, deleted);
   if `JIRA_CONFIG_PATH` set → parse file → populate `JiraProjects`.
   If unset but legacy env keys present → synthesize `JiraProjects` from
   `JIRA_PROJECT_ALLOWLIST` × `JIRA_READY_TO_DEV_STATUS_ID` (backward-compat
   window) with a warn log.
3. `Validate()`: dual-control error; every project key `[A-Z][A-Z0-9_]+`;
   every status ID numeric non-empty, no duplicates; site ID non-empty
   (whichever source carries it).
4. Startup log: `slog.Info("jira intake config", "source", "yaml|env",
   "projects", keys, "status_ids", ids)`.
5. Update `projectAllowed()` call site in phase 3 — here only config plumbing.

## Todo

- [ ] Loader + validation + tests (`internal/config` unit tests: dual control,
      empty allowlist, unknown key, numeric IDs)
- [ ] Delete `JiraReadyToDevStatusID` field + env; fix compile errors
- [ ] Example `jira.yaml` + runbook note (env→YAML migration steps)
- [ ] Compose volume mount + `JIRA_CONFIG_PATH` env wiring

## Success Criteria

- `JIRA_CONFIG_PATH` set + `JIRA_READY_TO_DEV_STATUS_ID` set → process exits
  non-zero with a clear error naming both keys.
- YAML with `VTX.start_status_ids: ["10007"]` → config exposes
  `JiraProjects["VTX"].StartStatusIDs == ["10007"]`; startup log shows it.
- `go vet ./...` clean; `go mod tidy` keeps yaml.v3 as direct dep.

## Risks

- Prod currently env-only → runbook must state the exact env-to-file migration
  before rollout (item lives in Phase 7 evidence).
- Strict `KnownFields` will break on typos — intentional (fail-closed).
