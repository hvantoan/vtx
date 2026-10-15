---
title: "Phase 4: Typed outbox event contract"
status: done
---

# Phase 4: Typed outbox event contract

## Overview

Closes review P1-4: the persisted `execution_intent.created.v1` payload is a
`map[string]any` missing `event_id`, `occurred_at`, `correlation_id`, and uses
no contract-aligned naming. Decision (user-confirmed): name the field
`changelog_id` — the value IS Jira `changelog.id`; the spec's `transition_id`
wording is corrected in the contract to avoid VTX-3 reading it as a Jira
workflow transition ID.

Priority: **P1** · Effort: ~2h · Depends on: Phase 3 (payload carries validated
fields)

## Requirements

- [ ] Typed struct `ExecutionIntentCreatedV1` — no `map[string]any`.
- [ ] Fields: `event_id` (inbox UUID), `intent_id`, `issue_id`, `issue_key`,
      `jira_site_id`, `changelog_id`, `target_status_id`, `occurred_at`
      (RFC3339 UTC), `correlation_id`. Optional `occurred_at_source`
      (`webhook_timestamp`|`received_at`) recommended for debuggability.
- [ ] `occurred_at`: from `env.Timestamp` (epoch ms per Jira webhook format —
      heuristic: value > 1e12 treat as ms, else seconds), normalized UTC;
      missing/zero/out-of-range → `received_at` + `occurred_at_source:
      received_at` reason. Never fabricate a third source.
- [ ] `received_at` captured once at handler entry and threaded into the
      service (receipts in Phase 6 reuse it).
- [ ] Contract schema added to `contracts/events/v1/events.json` (new
      `definitions.ExecutionIntentCreatedV1` + event type enum entry
      `execution_intent.created.v1`) and OpenAPI webhook docs mention the
      outbox contract version.
- [ ] No credentials, issue description, or raw payload in the event (spec §6).

## Related Files

- Modify: `apps/backend/internal/application/jira_webhook_service.go` — replace
  `map[string]any` marshal (lines 173–180) with the struct.
- Create: `apps/backend/internal/application/outbox_events.go` — typed contract
  + `OccurredAt(env *domain.JiraWebhookEnvelope, receivedAt time.Time)
  (ts time.Time, source string)` helper + tests.
- Modify: `contracts/events/v1/events.json` — add definition + enum member.
- Modify: `contracts/openapi/v1/openapi.yaml` — webhook description references
  `execution_intent.created.v1` field list.
- Modify: handler `JiraWebhookProcessor` signature — add `receivedAt
  time.Time` param (or pass via ctx; prefer explicit param).

## Implementation Steps

1. Define struct + `json` tags; `Marshal() ([]byte, error)`.
2. Parse `env.Timestamp`: `time.UnixMilli(ts).UTC()` when plausible (year
   2000..2100 sanity), else fallback.
3. `correlation_id = requestID` (already the service convention), `event_id =
   inbox.EventID` — order the insert so inbox row exists first (already does).
4. Update contracts + a conformance test: unmarshal a real inserted outbox
   payload against the JSON schema fields (field presence + types; no full
   schema-validation dep needed — assert required keys).
5. Comment in `jira.sql.go` is generated — document the contract in
   `contracts/` and the runbook, not in generated code.

## Todo

- [ ] Struct + marshal + wire `receivedAt` param
- [ ] `occurred_at` helper + edge tests (0, ms vs s, garbage)
- [ ] `contracts/events/v1/events.json` + `openapi.yaml` updates
- [ ] Conformance test on inserted payload

## Success Criteria

- Inserted outbox row's `payload` contains all 9 required keys; `changelog_id`
  present, `transition_id` absent.
- `occurred_at` equals webhook `timestamp` when valid; else equals
  `received_at` and `occurred_at_source` says so.
- VTX-3 consumer grep: `grep -rn "transition_id\|execution_intent" apps/
  contracts/` shows only the new contract — if any consumer already reads
  `transition_id`, **stop and ask** before renaming.

## Risks

- `env.Timestamp` zero when Jira omits it — covered by fallback.
- Contract file uses draft 2020-12; keep style consistent (no $ref sprawl).
