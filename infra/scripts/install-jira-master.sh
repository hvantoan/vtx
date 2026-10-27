#!/usr/bin/env bash
# ==============================================================================
# install-jira-master.sh — run the VTX Jira master poller as a systemd service.
#
#   sudo bash infra/scripts/install-jira-master.sh
#
# The poller is a plain Node.js loop (no LLM, no Temporal): every
# VTX_JIRA_POLL_INTERVAL_MS (default 30 min) it queries Jira through the
# mcp-atlassian MCP server configured in .pi/mcp.json, and appends newly
# discovered / changed issues to var/master/jira-queue.jsonl.
#
# State lives in var/master/ (gitignored):
#   jira-state.json    seen cursor (key -> version)
#   jira-queue.jsonl   append-only handoff queue for the orchestration phase
#
# The first tick only records the current matches (baseline) and queues
# nothing, so pre-existing tickets are never auto-dispatched.
#
# Prerequisites:
#   - `pnpm --filter @vtx/agent-runtime build` has produced dist/master/index.js
#   - .pi/mcp.json exists with the vtx-jira server (Jira URL + credentials)
#   - node is on the owner's PATH (override with NODE_BIN=/path/to/node)
#
# Override via env when calling the script:
#   VTX_JIRA_POLL_INTERVAL_MS, VTX_JIRA_PAGE_SIZE, VTX_JIRA_JQL,
#   VTX_MASTER_STATE_DIR, VTX_JIRA_MCP_CONFIG, VTX_JIRA_MCP_SERVER
#
# Uninstall:
#   sudo systemctl disable --now vtx-jira-master.service
#   sudo rm /etc/systemd/system/vtx-jira-master.service
#   sudo systemctl daemon-reload
# ==============================================================================
set -euo pipefail

if [[ ${EUID} -ne 0 ]]; then
  echo "❌ run with sudo: sudo bash infra/scripts/install-jira-master.sh" >&2
  exit 1
fi

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
OWNER="${SUDO_USER:-root}"
ENTRYPOINT="${REPO_ROOT}/apps/agent-runtime/dist/master/index.js"
UNIT_PATH="/etc/systemd/system/vtx-jira-master.service"
INTERVAL_MS="${VTX_JIRA_POLL_INTERVAL_MS:-1800000}"

if [[ ! -f "${ENTRYPOINT}" ]]; then
  echo "❌ ${ENTRYPOINT} is missing — run: pnpm --filter @vtx/agent-runtime build" >&2
  exit 1
fi

if [[ ! -f "${REPO_ROOT}/.pi/mcp.json" ]]; then
  echo "❌ ${REPO_ROOT}/.pi/mcp.json is missing (vtx-jira MCP server config)" >&2
  exit 1
fi

NODE_BIN="${NODE_BIN:-$(sudo -u "${OWNER}" bash -lc 'command -v node' || true)}"
if [[ -z "${NODE_BIN}" || ! -x "${NODE_BIN}" ]]; then
  echo "❌ node not found for user ${OWNER}; set NODE_BIN=/path/to/node" >&2
  exit 1
fi

# Preserve every VTX_* override the caller passed in.
ENV_LINES=("Environment=NODE_ENV=production" "Environment=VTX_JIRA_POLL_INTERVAL_MS=${INTERVAL_MS}")
for name in VTX_JIRA_PAGE_SIZE VTX_JIRA_JQL VTX_MASTER_STATE_DIR VTX_JIRA_MCP_CONFIG VTX_JIRA_MCP_SERVER VTX_MASTER_BASELINE; do
  if [[ -n "${!name:-}" ]]; then
    ENV_LINES+=("Environment=${name}=${!name}")
  fi
done

cat >"${UNIT_PATH}" <<UNIT
[Unit]
Description=VTX Jira master poller (MCP, every $((INTERVAL_MS / 60000)) minutes)
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=${OWNER}
WorkingDirectory=${REPO_ROOT}
ExecStart=${NODE_BIN} ${ENTRYPOINT}
$(printf '%s\n' "${ENV_LINES[@]}")
Restart=on-failure
RestartSec=30
NoNewPrivileges=true
PrivateTmp=true

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable --now vtx-jira-master.service
systemctl --no-pager --full status vtx-jira-master.service || true

echo
echo "✅ installed ${UNIT_PATH}"
echo "   logs: journalctl -u vtx-jira-master.service -f"
echo "   state: ${REPO_ROOT}/var/master"
