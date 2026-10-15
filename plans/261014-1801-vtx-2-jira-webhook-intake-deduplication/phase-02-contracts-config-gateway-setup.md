---
title: "Phase 2: Contracts, Config & Gateway Setup"
status: todo
---

# Phase 2: Contracts, Config & Gateway Setup

## Overview

Implement the Nginx Secure Webhook Gateway inside Docker Compose and define the OpenAPI contracts for the intake endpoint. Nginx does **not** read OS environment variables in `nginx.conf` — secret injection requires the official image's `envsubst` template mechanism (`/etc/nginx/templates/*.template`) with an explicit variable allowlist, or a startup-generated `map` block. `proxy_pass` inside a regex `location` must be URI-less; path stripping is done via `rewrite … break`. The backend's host port stays published for the web dashboard — the webhook route is protected by the dedicated internal token, not by removing the port.

## Requirements

- [x] Update `contracts/openapi/v1/openapi.yaml` with `POST /api/v1/integrations/jira/webhooks` — response schemas for **400**, 401, 413, 422, 200, 202, 503.
- [x] Create `infra/compose/nginx/` with `default.conf.template` (envsubst), TLS assets, and `gen_certs.sh` writing into a **gitignored** `certs/` dir (add `infra/compose/nginx/certs/` to `.gitignore`; widen the boundary scanner regex to `PRIVATE KEY` generically — `BEGIN PRIVATE KEY` PKCS#8 headers are not caught today).
- [x] Add a `gateway` service to `infra/compose/docker-compose.yml` on a shared non-`internal:true` network with `backend`, `postgres`, `temporal` (backend needs outbound egress for Jira re-fetch).
- [x] Secret rotation: Nginx `map` accepts `WEBHOOK_PATH_SECRET_PRIMARY` and `WEBHOOK_PATH_SECRET_SECONDARY`; rotation runbook uses overlap window (deploy gateway accepting both, then update Jira, then retire old — never reverse order).
- [x] **Keep** `${VTX_API_PORT:-49080}:49080` published — the Next.js dashboard calls `http://localhost:49080` from the browser (SSE `/api/v1/events` + REST). Webhook-route protection comes from the dedicated `GATEWAY_INTERNAL_AUTH_TOKEN` (validated as distinct from `API_AUTH_TOKEN`), not port removal.

## Implementation Steps

1. **OpenAPI Contract**: Define `WebhookResponse` (`request_id` ← `middleware.GetReqID`, `disposition`, `intent_id`) and error bodies for 400 (malformed body / wrong Content-Type), 401, 413, 422, 200, 202, 503.
2. **Nginx config (`default.conf.template`)**:
   - `envsubst` allowlist: render only `${WEBHOOK_PATH_SECRET_PRIMARY}`, `${WEBHOOK_PATH_SECRET_SECONDARY}`, `${GATEWAY_INTERNAL_AUTH_TOKEN}`, `${VTX_API_PORT}` — everything else stays as nginx variables.
   - `map $uri $webhook_secret_ok` mapping `/webhooks/<secret>/jira` paths to `1` when the secret matches primary or secondary (templated literal compare via `map`, **not** `if` inside `location`).
   - `location ^~ /webhooks/` block: `rewrite ^/webhooks/[^/]+/jira$ /api/v1/integrations/jira/webhooks break;` then URI-less `proxy_pass http://backend:${VTX_API_PORT};` (regex `location` + URI-bearing `proxy_pass` fails `nginx -t`; `rewrite … break` + bare `proxy_pass` is the working form — and it prevents the secret path from reaching Go logs).
   - `proxy_set_header Authorization "Bearer ${GATEWAY_INTERNAL_AUTH_TOKEN}";` — overwrite unconditionally (`proxy_set_header` replaces, never forwards, the client header).
   - `limit_req_zone`/`limit_req` on the webhook location (rate limiting per spec §3.3).
   - Custom `log_format` emitting `$masked_uri` (secret replaced with `***`), never raw `$request_uri`.
   - 403 `return` for non-matching secrets before any `proxy_pass`.
3. **Compose**: `gateway` service (`nginx:stable-alpine`), port `${VTX_GATEWAY_PORT:-49443}:443`, mounts template to `/etc/nginx/templates/`, certs volume from `./certs`, env vars passed through, `depends_on: backend`. Define explicit `networks:` (shared bridge for backend+gateway+postgres+temporal; keep backend egress for `*.atlassian.net`). Keep the existing `49080` host mapping.
4. **TLS certs**: `gen_certs.sh` generates self-signed cert+key into `infra/compose/nginx/certs/` (gitignored) — never committed.

## Todo

- [x] Write OpenAPI endpoint spec incl. 400
- [x] Create `default.conf.template` with envsubst allowlist + `map` + `rewrite break`
- [x] Write `gen_certs.sh` → gitignored `certs/`, add `.gitignore` entry, widen scan regex
- [x] Update docker-compose.yml (gateway service + shared network, keep 49080 published)
- [x] Document rotation overlap procedure

## Success Criteria

- `docker compose up gateway backend` starts; `nginx -t` passes inside the gateway container.
- `curl -k https://localhost:49443/webhooks/WRONG_SECRET/jira` → 403 from Nginx, never reaching Go.
- `curl -k https://localhost:49443/webhooks/<PRIMARY>/jira` reaches Go backend (404/501 acceptable at this phase) — and the Go access log shows `/api/v1/integrations/jira/webhooks`, never `/webhooks/`.
- `https://localhost:49080/api/v1/...` still serves the dashboard endpoints (regression check).
