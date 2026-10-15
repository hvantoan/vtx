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
| `JIRA_READY_TO_DEV_STATUS_ID` | **`10007`** ("Ready To Develope", all issue types in VTX) | `GET /rest/api/3/project/VTX/statuses` |

> ⚠️ The Jira UI's transition `id=2` for "Ready To Develope" is the
> **transition** ID, not the status ID. Status IDs on this tenant are `10000+`.
> Re-verify before any status change:
> `curl -u "$JIRA_API_EMAIL:$JIRA_API_TOKEN" "$JIRA_BASE_URL/rest/api/3/status"`

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
| 202 | `ignored_stale` | Jira re-fetch: issue no longer in target status |
| 202 | `ignored_scope` | non-status event / project outside allowlist / missing changelog |
| 422 | `invalid_transition` | structurally valid but unprocessable envelope |
| 400 | — | wrong Content-Type or malformed JSON |
| 401 | — | missing/invalid internal token |
| 413 | — | payload > 256 KiB |
| 503 | — | Jira API or DB transient failure; Jira redelivers |

## Outbox contract (for VTX-3 consumers)

Every accepted transition publishes exactly one row:

- `outbox_events.event_type` = `execution_intent.created.v1`
- `outbox_events.intent_id` → `execution_intents.intent_id` (`status='pending'`)
- `payload` JSONB: `intent_id`, `issue_key`, `issue_id`, `jira_site_id`,
  `changelog_id`, `target_status_id` — sanitized; no credentials, no raw body.
- `published_at IS NULL` marks undelivered events
  (`idx_outbox_events_unpublished`). A consumer polls, publishes, then sets
  `published_at`. Relay implementation is VTX-3 scope.

## Failure modes & recovery

| Symptom | Cause | Recovery |
|---|---|---|
| 403 at gateway | wrong/stale path secret | check Jira webhook URL vs `WEBHOOK_PATH_SECRET_PRIMARY` |
| 401 from backend | gateway env mismatch | `GATEWAY_INTERNAL_AUTH_TOKEN` must match in gateway + backend |
| Sustained 503 | Jira API down / DB down | Jira auto-redelivers; fix upstream; no manual replay needed |
| `startup fail: GATEWAY_INTERNAL_AUTH_TOKEN …` | token unset or equals `API_AUTH_TOKEN` | set a distinct high-entropy token |
| Duplicate intents for one transition | impossible by unique index — investigate `changelog_id` reuse | inspect `jira_webhook_inbox` rows |

## Secret hygiene

- Path secret lives only in the nginx `map`; `rewrite … break` strips it
  before proxying. Access logs emit `$masked_uri` (`/webhooks/***/jira`).
- `GATEWAY_INTERNAL_AUTH_TOKEN` is gateway-injected, never client-supplied,
  and asserted distinct from `API_AUTH_TOKEN` at backend startup.
- `infra/compose/nginx/certs/` is gitignored; the boundary scanner
  (`check-boundaries.sh`) greps all `PRIVATE KEY` PEM forms.
