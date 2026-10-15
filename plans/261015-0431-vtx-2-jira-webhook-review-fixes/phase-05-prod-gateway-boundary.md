---
title: "Phase 5: Production gateway boundary (compose overlay)"
status: done
---

# Phase 5: Prod gateway boundary

## Overview

Closes review P1-3: `docker-compose.yml` publishes `49080:49080`, so the
backend's webhook route (authenticated by `GATEWAY_INTERNAL_AUTH_TOKEN`
alone) is reachable without traversing nginx — the gateway contract is a
bearer token, not a network boundary. Fix is a production overlay that
removes the public binding; dev compose stays unchanged.

Verified during planning: nginx already does the hard part —
`proxy_set_header Authorization "Bearer ${GATEWAY_INTERNAL_AUTH_TOKEN}"`
overwrites any client-sent auth, `rewrite … break` strips the secret path
before proxying, access logs mask the URI (`/webhooks/***/…`), per-IP rate
limit `10r/s burst=20`, secondary-secret rotation map, TLS only. **No nginx
changes needed.**

Priority: **P1** · Effort: ~1.5h · Independent of Phases 1–4, 6

## Requirements

- [ ] `docker-compose.prod.yml` overlay: `backend` has **no** `ports` — reachable
      only on the internal compose network via `backend:49080` from `gateway`.
- [ ] `postgres`, `temporal`, `temporal-ui`, `web` ports also audited in the
      overlay — the overlay is the production profile; anything not required
      publicly gets no host binding (recommend: only `gateway` keeps a port).
      If `web` must stay public in prod, state the reason in the file header.
- [ ] Overlay must not duplicate the whole service block — compose merges
      `ports` by replacement; use `ports: !reset []` (Compose spec ≥ 2.24.4 /
      `docker compose` v2.24+) or `!override`; verify with `docker compose
      config`. If the runtime's compose version lacks `!reset`, fall back to a
      standalone `docker-compose.prod.yml` that includes base via `include:`
      or fully re-declares backend — pick whichever `docker compose config`
      renders correctly.
- [ ] Runbook note: prod deployment command is
      `docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d`.

## Related Files

- Create: `infra/compose/docker-compose.prod.yml`
- Modify: `infra/compose/README.md` or runbook doc (find existing docs home —
  do not invent a second one)
- Read-only verify: `infra/compose/nginx/default.conf.template` (already
  correct — re-confirm no client-auth passthrough on the webhook location)

## Implementation Steps

1. Write overlay removing `backend.ports` and any other unnecessary public
   bindings.
2. `docker compose -f docker-compose.yml -f docker-compose.prod.yml config`
   → assert `backend` has no `ports`; `gateway` still publishes 443.
3. `docker compose -f … config` output pasted into the JT-16 evidence (direct
   bypass now impossible — nothing to connect to).
4. Keep dev flow intact: `docker compose up` (base only) still publishes
   `49080` for local web dev.

## Todo

- [ ] Overlay file + `docker compose config` verification
- [ ] Doc/runbook update with prod invocation
- [ ] Confirm `web`/`postgres`/`temporal` port policy in prod profile

## Success Criteria

- `docker compose -f docker-compose.yml -f docker-compose.prod.yml config
  --services` works; rendered `backend` has zero `ports` entries.
- From the host: `curl localhost:49080/api/v1/integrations/jira/webhooks` →
  connection refused under the prod profile (evidence for JT-16).

## Risks

- `ports: !reset` unsupported on older compose → fallback documented above.
- Healthchecks/LB outside compose may target `49080` — flag in runbook if any
  deployment tooling assumes the port.
