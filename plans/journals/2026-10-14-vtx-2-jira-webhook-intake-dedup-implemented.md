---
title: "VTX-2 Jira webhook intake & dedup implemented"
date: 2026-10-14
summary: "Full 6-phase plan shipped: nginx gateway + Go intake + PG dedup; 19/19 E2E PASS; live status ID 10007 verified"
---

# VTX-2 Jira webhook intake & dedup implemented

## What happened
Implemented the complete VTX-2 plan (SPEC-AUTO-01 Amendment 01): secure Jira Cloud webhook intake with deduplication.

- Phase 1: verified live tenant `Ready To Develope` = status ID **10007** via `GET /rest/api/3/project/VTX/statuses` (transition id=2 is NOT the status ID). Extended `config.go` with 6 Jira/gateway keys + `Validate()` (fail-closed: internal token required and != API_AUTH_TOKEN). Created `.env`, env wiring into compose, webhook fixtures.
- Phase 2: OpenAPI endpoint + `WebhookResponse`/`JiraWebhookEnvelope` schemas; `default.conf.template` with envsubst allowlist, `map` secret check, `rewrite … break`, masked log format, `limit_req`; `gen_certs.sh` to gitignored `certs/`; boundary regex widened to `PRIVATE KEY` (caught PKCS#8); gateway service + shared `vtx_net` network; `--env-file` in Makefile compose targets.
- Phase 3: migration 00002 (`jira_webhook_inbox`, `execution_intents`, `outbox_events`, Up+Down verified); sqlc queries + generated querier; `JiraWebhookEnvelope` with `StatusTransitionTo()` scan.
- Phase 4: Jira client (SSRF guards, ≤3s budget, fail-fast on 429/5xx — never sleeps Retry-After); service with single `pgx.Tx` (`WithTx`) binding inbox+intent+outbox+outcome update; retryable errors return before any insert so the dedup key stays unclaimed.
- Phase 5: `HandleJiraWebhook` (Content-Type 400, 256KiB 413, 202-for-ignorable policy, 503 on ErrRetryable); `GatewayAuthMiddleware` on dedicated token; route outside dashboard auth group; `WithJiraWebhook` wiring in cmd/api.
- Phase 6: `test-jira-webhook.sh` JT-01..JT-18 (19 checks incl. 2-replica concurrency, secret-leak audit, rotation overlap); `tests/evidence/jira-webhook-intake.log` 19/19 PASS; `docs/runbooks/jira-webhook-intake.md`.

## Decisions & incidents
- Nginx gotchas found by E2E: `map_hash_bucket_size 128` needed for 48-hex secrets; `set $upstream` must precede `rewrite … break` (break ends rewrite phase → uninitialized var → 500); literal `proxy_pass` caches upstream DNS → 502 after `--scale`, fixed with `resolver 127.0.0.11` + variable `proxy_pass`.
- `container_name` removed from backend so `--scale backend=2` works (JT-03); `docker-compose.e2e-scale.yml` uses `ports: !reset []` to unpublish when scaling.
- `JIRA_BASE_URL` https required except internal/loopback hosts, enabling the `jira-stub` compose service for failure injection.
- Boundary scanner now excludes `certs/` (gitignored generated keys are the intended output, not a leak).

## Verification
`make migrate-up/down` clean; `sqlc generate` produced 5 queries; unit tests green (8-way concurrent dedup, 0-row retryable, token separation incl. API token rejected on webhook route); E2E 19/19 PASS against compose stack; `make test-boundaries vet typecheck test build` all green.

## Next steps
VTX-3: outbox relay consuming `execution_intent.created.v1` (`published_at IS NULL` index ready). Production: real TLS cert, Jira Admin webhook pointing at `/webhooks/<PRIMARY>/jira`.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
