# Runbook: Jira Webhook Intake & Deduplication (VTX-2 / SPEC-AUTO-01)

Operations guide for the Jira Cloud webhook intake: Secure Webhook Gateway,
Go intake service, PostgreSQL dedup, and transactional outbox.

## Architecture

```
Jira Cloud ──HTTPS──▶ gateway (nginx :49443) ──▶ backend (:49080)
  Issue updated      layer 1: path secret          layer 2: internal bearer
  + changelog        map PRIMARY/SECONDARY          GatewayAuthMiddleware
                     rewrite → /api/v1/…            tx: inbox+intent+outbox
```

- **Transition key (dedup):** `(jira_site_id, issue_id, changelog_id, target_status_id)`
- **Single transaction:** accepted events commit `jira_webhook_inbox` +
  `execution_intents` + `outbox_events` atomically (pgx.Tx via `WithTx`).
- **Retryable failures → 503 with NO inbox row.** The dedup key is never
  claimed by a failed attempt; Jira's redelivery reprocesses cleanly.
- **Ignorable traffic → 202 always.** Non-status events, out-of-scope
  projects, missing changelog — sustained 4xx gets the webhook auto-disabled.

## Verified tenant configuration

| Setting | Value | Verified |
|---|---|---|
| `JIRA_BASE_URL` | `https://vtxsolution.atlassian.net` | 2026-10-15 |
| `JIRA_SITE_ID` | `vtxsolution.atlassian.net` | 2026-10-15 |
| `start_status_ids` (project VTX) | **`10007`** ("Ready To Develope", all issue types) | `GET /rest/api/3/project/VTX/statuses` |

> ⚠️ The Jira UI's transition `id=2` for "Ready To Develope" is the
> **transition** ID, not the status ID. Status IDs on this tenant are `10000+`.
> Re-verify before any status change:
> `curl -u "$JIRA_API_EMAIL:$JIRA_API_TOKEN" "$JIRA_BASE_URL/rest/api/3/status"`

## Intake policy config (project → start_status_ids)

Single source of truth — pick exactly one, never both:

1. **YAML (production path):** `infra/compose/config/jira.yaml`, mounted as
   `JIRA_CONFIG_PATH=/etc/vtx/jira.yaml`. Strict validation at startup:
   unparseable file, unknown keys, empty projects, empty/duplicate/non-numeric
   `start_status_ids` → fatal, fail-closed.
2. **Legacy env (migration window):** `JIRA_PROJECT_ALLOWLIST` ×
   `JIRA_READY_TO_DEV_STATUS_ID` — only valid while `JIRA_CONFIG_PATH` is
   unset. Setting legacy keys alongside the path is a startup error.

### env → YAML migration (production rollout order)

1. Write `jira.yaml` with the resolved `start_status_ids` (verify against the
   tenant first — see table above).
2. Deploy with `JIRA_CONFIG_PATH` set AND the legacy keys **removed** in the
   same release. Do not deploy the path alone while legacy keys remain —
   startup will abort on dual control by design.
3. Confirm the startup log line `jira intake config source=yaml
   projects=[...] status_ids=[...]` on the first healthy boot.
4. Rollback = revert to the previous release (env keys restored).

## Jira Admin setup

1. **System → Webhooks** (Jira Cloud Free: admin menu availability varies by
   tenant — confirm before assuming).
2. Create webhook:
   - **URL:** `https://<public-host>/webhooks/<WEBHOOK_PATH_SECRET_PRIMARY>/jira`
   - **Events:** `Issue updated` (with changelog)
   - **JQL filter:** `project = VTX`
3. No custom headers needed — authentication is the URL path secret (layer 1)
   plus the gateway-injected internal token (layer 2).

## Local stack

```bash
cp .env.example .env   # fill JIRA_* / GATEWAY_* / WEBHOOK_* values
infra/compose/nginx/gen_certs.sh        # self-signed dev TLS (gitignored)
make compose-up                          # postgres, temporal, backend, gateway, web
```

E2E test matrix (JT-01..JT-18) with a mocked Jira API:

```bash
./infra/scripts/test-jira-webhook.sh | tee tests/evidence/jira-webhook-intake.log
```

## Secret rotation (overlap — never reverse this order)

1. Set `WEBHOOK_PATH_SECRET_SECONDARY=<new-secret>` in `.env`.
2. `make compose-up` — gateway reloads accepting **both** secrets.
3. Update the webhook URL in Jira Admin to the new secret path.
4. Confirm deliveries arrive on the new path (`outcome='accepted'` rows,
   gateway masked access log).
5. Clear `WEBHOOK_PATH_SECRET_SECONDARY`, rotate `PRIMARY` to the new value,
   `make compose-up`. Only then is the old secret retired — zero delivery loss.

Same overlap rule applies to `GATEWAY_INTERNAL_AUTH_TOKEN` rotation (deploy
backend accepting new token, then update gateway env, then restart gateway).

## Response / disposition map

| HTTP | disposition | meaning |
|---|---|---|
| 202 | `accepted` | intent + outbox committed |
| 200 | `duplicate` | same transition redelivered; returns existing `intent_id` |
| 202 | `ignored_stale` | Jira re-fetch: issue no longer in a start status |
| 202 | `ignored_scope` | non-status event / project not configured / missing changelog / authoritative identity mismatch (forged payload) |
| 422 | `invalid_transition` | transition target is not a configured `start_status_id` — receipt only, never an inbox row or intent |
| 400 | — | wrong Content-Type or malformed JSON |
| 401 | — | missing/invalid internal token |
| 413 | — | payload > 256 KiB |
| 503 | — | Jira API or DB transient failure; Jira redelivers |

## Outbox contract (for VTX-3 consumers)

Every accepted transition publishes exactly one row:

- `outbox_events.event_type` = `execution_intent.created.v1`
- `outbox_events.intent_id` → `execution_intents.intent_id` (`status='pending'`)
- `payload` JSONB: `event_id`, `intent_id`, `issue_id`, `issue_key`,
  `jira_site_id`, `changelog_id`, `target_status_id`, `occurred_at`
  (RFC3339 UTC — the webhook envelope timestamp, or `received_at` when the
  timestamp is missing/implausible; `occurred_at_source` says which),
  `correlation_id` — sanitized; no credentials, no raw body. `changelog_id`
  is the Jira changelog.id, **not** a workflow transition ID.
- `published_at IS NULL` marks undelivered events
  (`idx_outbox_events_unpublished`). A consumer polls, publishes, then sets
  `published_at`. Relay implementation is VTX-3 scope.

## Audit receipts (`webhook_receipts`)

Every authenticated request — including malformed bodies and wrong-target
transitions that never reach `jira_webhook_inbox` — appends one row to
`webhook_receipts` (outcome + `reason_code` + `request_id` + `payload_hash`,
`issue_key` when the envelope parsed). Written best-effort after processing:
a receipt failure logs an error, never changes the response. 401/403 requests
are rejected by `GatewayAuthMiddleware` before the handler and write nothing.

Retention: `ignored_scope`/`invalid_transition`/`rejected` rows older than 7
days are deleted by a 6h ticker in `cmd/api`. `accepted`/`duplicate`/
`ignored_stale`/`failed_retryable` rows and all `jira_webhook_inbox` rows are
retained as the audit record.

## Production deployment

```bash
docker compose --env-file .env \
  -f infra/compose/docker-compose.yml \
  -f infra/compose/docker-compose.prod.yml up -d
```

The overlay removes every host port except the gateway's (49443). The backend
is reachable only on the internal network — the webhook route's bearer token
is a credential, not a boundary, so a published `49080` would bypass layer-1
path-secret validation. Verify before/after deploy:

```bash
docker compose --env-file .env -f infra/compose/docker-compose.yml \
  -f infra/compose/docker-compose.prod.yml config | grep -A2 'ports:'
# expected: only gateway publishes 49443 -> 443
curl -s http://localhost:49080/api/v1/integrations/jira/webhooks
# expected: connection refused under the prod profile
```

## Failure modes & recovery

| Symptom | Cause | Recovery |
|---|---|---|
| 403 at gateway | wrong/stale path secret | check Jira webhook URL vs `WEBHOOK_PATH_SECRET_PRIMARY` |
| 401 from backend | gateway env mismatch | `GATEWAY_INTERNAL_AUTH_TOKEN` must match in gateway + backend |
| Sustained 503 | Jira API down / DB down | Jira auto-redelivers; fix upstream; no manual replay needed |
| `startup fail: GATEWAY_INTERNAL_AUTH_TOKEN …` | token unset or collides with a machine actor token | set a distinct high-entropy token |
| Duplicate intents for one transition | impossible by unique index — investigate `changelog_id` reuse | inspect `jira_webhook_inbox` rows |

## Secret hygiene

- Path secret lives only in the nginx `map`; `rewrite … break` strips it
  before proxying. Access logs emit `$masked_uri` (`/webhooks/***/jira`).
- `GATEWAY_INTERNAL_AUTH_TOKEN` is gateway-injected, never client-supplied,
  and asserted distinct from every `REGISTRY_ACTOR_*` token at backend
  startup. Legacy public API bearer tokens are rejected; operators sign in
  with a session cookie instead.
- `infra/compose/nginx/certs/` is gitignored; the boundary scanner
  (`check-boundaries.sh`) greps all `PRIVATE KEY` PEM forms.
