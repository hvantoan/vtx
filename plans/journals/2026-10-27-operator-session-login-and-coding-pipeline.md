---
title: operator session login + coding pipeline shipment
date: 2026-10-27
summary: "Replaced the shared operator bearer token with app-hosted username/password sessions and a same-origin web proxy; shipped the previously uncommitted master poller / spine / orchestrator pipeline in the same beta PR because the two efforts shared files."
---

# operator session login + coding pipeline shipment

## What happened

Two streams of completed work were uncommitted on `dev` at the same time:

1. **Coding pipeline (earlier sessions).** Jira master poller over MCP, the
   `worker spine` intake tool server, the Pi orchestrator extension and
   `worker orchestrate` launcher, plus worktree/verification regressions found
   while running the pipeline end to end.
2. **Operator login (this session).** The shared `REGISTRY_ACTOR_OPERATOR_TOKEN`
   / `API_AUTH_TOKEN` model was replaced with an app-hosted login: one seeded
   account, Argon2id password hashes, opaque server sessions in `auth_sessions`,
   a `vtx_session` HttpOnly cookie, sliding expiry capped at a one-hour
   extension per call, per-IP login throttling, and logout/password-change
   revocation.

Both streams changed the same files (`config.go`, the sqlc `Querier` interface,
`Makefile`, `cmd/worker/main.go`, compose), so a partial commit would not
compile. They ship together as one beta PR instead of inventing an untested
split.

## Decisions

- **Sessions over a break-glass token.** Per the interview, the operator bearer
  token was removed entirely; machines keep scheduler/orchestrator/policy tokens
  and sessions map to `domain.ActorOperator`. Legacy dashboard and operator
  tokens now return 401 everywhere, including the Jira webhook route.
- **Same-origin runtime proxy.** `apps/web` proxies `/api/*`, `/health` and
  `/ready` to `INTERNAL_API_URL` from a Next runtime route handler rather than
  `next.config` rewrites, because rewrites bake their destination at build time
  and the tunnel and LAN must share one image.
- **Identity from the session.** `X-Operator-Id` is ignored; audit attribution
  comes from the authenticated session username.
- **OpenAPI updated in the same change.** Added `/api/v1/auth/*`, the
  `OperatorSession` cookie scheme, and session alternatives on the operator and
  SSE surfaces; `RegistryBearer` now documents three machine actors.

## Verification

- `make test-boundaries`, `make vet`, `make typecheck`, `make lint`, `make test`,
  `make build` all pass. Go: 356 tests; agent-runtime: 151; web: 11.
- Live compose checks: login 200 with HttpOnly cookie, `/auth/me` 200, protected
  routes 401 without cookie / 200 with, legacy bearer 401, SSE streams through
  the Next proxy, password change 204 and revokes other sessions, logout 204,
  five bad passwords then 429.
- Headless Chromium walkthrough: guard redirect, login, console, settings,
  logout and post-logout guard all passed (7/7).
- `npx @redocly/cli lint contracts/openapi/v1/openapi.yaml` is valid.

## Caveats

- Tunnel HTTPS login was not exercised live; the proxy path and direct HTTP were.
- The dev database's operator password was set to a throwaway value during
  verification. Run `make user-password` before using the console.
- Login throttling is in-process per replica; it is not a distributed limiter.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
