# Remote Access via Cloudflare Tunnel

The vtx stack is exposed through the existing **homelab** Cloudflare Tunnel
(`d8937a30-…`, `*.hvantoan.xyz`, systemd unit `cloudflared.service`,
config `/etc/cloudflared/config.yml`).

A second connector (docker container `cloudflared-tunnel`, reading
`~/.cloudflared/config.yml`) used to serve the same tunnel and caused
intermittent 404s from config drift; it is stopped with `--restart=no`.
If it is ever re-enabled, keep both config files in sync —
`infra/scripts/setup-vtx-tunnel.sh` patches both. Check stray connectors
with `cloudflared tunnel info homelab`.

## Layout — single hostname, path-routed

| URL | Target |
|---|---|
| `https://vtx.hvantoan.xyz` | `http://localhost:49030` — Next.js dashboard |
| `https://vtx.hvantoan.xyz/api/*` | `http://localhost:49080` — backend API + SSE (`/api/v1/events`) |
| `https://vtx.hvantoan.xyz/health`, `/ready` | `http://localhost:49080` — probes used by the dashboard |

Same-origin routing means **no backend CORS change** and no separate API
hostname. Operators sign in once at `/login`; the API sets an HttpOnly,
SameSite=Lax `vtx_session` cookie that the browser sends on every `/api/*`
request. Machine principals (scheduler/orchestrator/policy) still use their
own bearer tokens. The webhook gateway on `:49443` is **not** exposed (add an
ingress rule for `vtx.hvantoan.xyz/webhooks/*` → `https://localhost:49443`
with `originRequest.noTLSVerify: true` if real Jira ingress is ever needed —
the path secret + internal token already gate it).

## One-time setup

```bash
# 1. DNS CNAME -> homelab tunnel (user-level, no sudo)
cloudflared tunnel route dns homelab vtx.hvantoan.xyz

# 2. Add ingress rules + restart cloudflared (sudo; idempotent, backs up config)
sudo bash infra/scripts/setup-vtx-tunnel.sh

# 3. Start the stack (also seeds the schema, then set the operator password)
make compose-up
make user-password
```

No web rebuild is needed when switching between the tunnel and LAN: the
dashboard calls `/api/*` on its own origin at runtime, and the Next.js server
proxies those requests to `INTERNAL_API_URL` (compose sets
`http://backend:49080`). Through the tunnel, `/api/*` is routed directly to
the backend by cloudflared; both paths use the same session cookie.

Set `SESSION_COOKIE_SECURE=auto` (the default) so the cookie is marked
`Secure` when Cloudflare forwards `X-Forwarded-Proto: https`, while plain-HTTP
LAN access still works.

## Verify

```bash
curl -s https://vtx.hvantoan.xyz/health          # -> backend JSON
curl -s -o /dev/null -w '%{http_code}' https://vtx.hvantoan.xyz/   # -> 200
curl -s -o /dev/null -w '%{http_code}' https://vtx.hvantoan.xyz/api/v1/auth/me  # -> 401
cloudflared --config /etc/cloudflared/config.yml tunnel ingress rule \
  https://vtx.hvantoan.xyz/api/v1/system/status  # shows matched rule
```

Then sign in at `https://vtx.hvantoan.xyz/login` — the cookie should be
`Secure` in the browser, and `GET /api/v1/auth/me` should return the operator
identity.

## Caveats

- LAN browsing at `http://<host>:49030` is fully independent of the tunnel:
  the browser talks only to Next.js, which proxies `/api/*` to the backend on
  the compose network. The same image and cookie flow serve both addresses.
- `cloudflared` path rules support `*` wildcards; `/api/*` covers all
  `/api/v1/**` endpoints. Health/ready need explicit exact-path rules.
- The login limiter is in-process per API replica (5 failures per client
  IP + username per 15 minutes). Behind the Next.js proxy all LAN clients
  share the proxy's forwarded address; through the tunnel Cloudflare supplies
  `X-Forwarded-For`.
