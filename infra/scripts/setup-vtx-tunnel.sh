#!/usr/bin/env bash
# ==============================================================================
# setup-vtx-tunnel.sh — expose the vtx stack through the existing "homelab"
# Cloudflare Tunnel on vtx.hvantoan.xyz (single hostname, path-routed).
#
#   sudo bash infra/scripts/setup-vtx-tunnel.sh
#
# Routes added to /etc/cloudflared/config.yml (homelab tunnel d8937a30).
# NOTE: a second connector — docker container "cloudflared-tunnel" — serves
# the SAME tunnel but reads ~/.cloudflared/config.yml. This script patches
# both files and restarts both, otherwise Cloudflare round-robins requests
# to the stale replica and they 404.
#   vtx.hvantoan.xyz/api/*   -> http://localhost:49080  (backend API + SSE)
#   vtx.hvantoan.xyz/health  -> http://localhost:49080
#   vtx.hvantoan.xyz/ready   -> http://localhost:49080
#   vtx.hvantoan.xyz/*       -> http://localhost:49030  (Next.js dashboard)
#
# Because the API is served on the same origin as the dashboard, no backend
# CORS change is needed; API routes are protected by the operator session
# cookie (HttpOnly, SameSite=Lax) set by /api/v1/auth/login.
#
# The DNS CNAME for vtx.hvantoan.xyz must already exist:
#   cloudflared tunnel route dns homelab vtx.hvantoan.xyz
#
# No rebuild is needed when switching between tunnel and LAN: the dashboard
# always calls /api on its own origin at runtime.
# ==============================================================================
set -euo pipefail

CONF=/etc/cloudflared/config.yml
HOST=vtx.hvantoan.xyz

if [[ $EUID -ne 0 ]]; then
  echo "error: run with sudo (modifies $CONF and restarts cloudflared)" >&2
  exit 1
fi

if grep -q "hostname: ${HOST}" "$CONF"; then
  echo "==> ingress rules for ${HOST} already present, skipping edit"
else
  BACKUP="${CONF}.bak-vtx-$(date +%Y%m%d-%H%M%S)"
  cp "$CONF" "$BACKUP"
  echo "==> backed up $CONF -> $BACKUP"

  python3 - "$CONF" <<'PYEOF'
import sys

conf_path = sys.argv[1]
with open(conf_path) as f:
    lines = f.readlines()

block = [
    "  - hostname: vtx.hvantoan.xyz\n",
    "    path: /api/*\n",
    "    service: http://localhost:49080\n",
    "  - hostname: vtx.hvantoan.xyz\n",
    "    path: /health\n",
    "    service: http://localhost:49080\n",
    "  - hostname: vtx.hvantoan.xyz\n",
    "    path: /ready\n",
    "    service: http://localhost:49080\n",
    "  - hostname: vtx.hvantoan.xyz\n",
    "    service: http://localhost:49030\n",
]

# Insert before the catch-all (the last "  - service: http_status:404").
idx = None
for i, line in enumerate(lines):
    if line.strip() == "- service: http_status:404":
        idx = i
if idx is None:
    sys.exit("error: no http_status:404 catch-all found in " + conf_path)

lines[idx:idx] = block
with open(conf_path, "w") as f:
    f.writelines(lines)
print("==> inserted vtx ingress rules")
PYEOF
fi

echo "==> validating ingress config"
cloudflared --config "$CONF" tunnel ingress validate

echo "==> restarting cloudflared"
systemctl restart cloudflared
sleep 2
systemctl is-active --quiet cloudflared && echo "==> cloudflared active"

# Second connector: docker container "cloudflared-tunnel" mounts
# ~/.cloudflared as /etc/cloudflared, i.e. it serves ~/.cloudflared/config.yml.
HOME_CONF="${SUDO_HOME:-$HOME/.cloudflared}/config.yml"
[ -n "${SUDO_USER:-}" ] && HOME_CONF="$(getent passwd "$SUDO_USER" | cut -d: -f6)/.cloudflared/config.yml"
if docker ps --format '{{.Names}}' | grep -qx cloudflared-tunnel && [ -f "$HOME_CONF" ]; then
  if ! grep -q "hostname: ${HOST}" "$HOME_CONF"; then
    cp "$HOME_CONF" "${HOME_CONF}.bak-vtx-$(date +%Y%m%d-%H%M%S)"
    python3 - "$HOME_CONF" <<'PYEOF'
import sys
conf_path = sys.argv[1]
with open(conf_path) as f:
    lines = f.readlines()
block = [
    "  - hostname: vtx.hvantoan.xyz\n",
    "    path: /api/*\n",
    "    service: http://localhost:49080\n",
    "  - hostname: vtx.hvantoan.xyz\n",
    "    path: /health\n",
    "    service: http://localhost:49080\n",
    "  - hostname: vtx.hvantoan.xyz\n",
    "    path: /ready\n",
    "    service: http://localhost:49080\n",
    "  - hostname: vtx.hvantoan.xyz\n",
    "    service: http://localhost:49030\n",
]
idx = None
for i, line in enumerate(lines):
    if line.strip() == "- service: http_status:404":
        idx = i
if idx is None:
    sys.exit("error: no http_status:404 catch-all in " + conf_path)
lines[idx:idx] = block
with open(conf_path, "w") as f:
    f.writelines(lines)
print(f"==> inserted vtx rules into {conf_path}")
PYEOF
  fi
  echo "==> restarting docker replica cloudflared-tunnel"
  docker restart cloudflared-tunnel >/dev/null
fi

echo "==> smoke checks (DNS may take a moment to propagate)"
for url in "https://${HOST}/health" "https://${HOST}/"; do
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 15 "$url" || true)
  echo "    ${url} -> HTTP ${code}"
done
