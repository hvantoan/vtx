#!/usr/bin/env bash
# ==============================================================================
# VTX Platform — one-command host bootstrap (Debian/Ubuntu + Fedora)
#
#   git clone https://github.com/hvantoan/vtx.git ~/vtx
#   ~/vtx/infra/scripts/setup-vtx-host.sh
#
# What it does (idempotent — safe to re-run):
#   1. Detects apt/dnf (fails fast elsewhere)
#   2. Installs base packages + Docker Engine with the compose plugin
#   3. Generates .env from .env.example with random secrets (only empty
#      or known-placeholder values are touched)
#   4. Jira intake prompt (interactive only): base URL / site / API email /
#      API token and project→start-status mapping → .env + generated
#      infra/compose/config/jira.local.yaml (mounted via JIRA_CONFIG_FILE).
#      Skipped non-interactively or when real credentials already exist.
#   5. Generates the nginx webhook-gateway TLS cert (self-signed, dev)
#   6. docker compose up -d (dev profile — dashboard/API reachable)
#   7. Waits for backend /health, installs goose, runs DB migrations
#   8. Host-side agent runtime: fnm + Node (repo .node-version) + corepack
#      + pnpm build of apps/agent-runtime + pi CLI
#   9. Prints URLs and the manual checklist
#
# Flags:
#   --check        verify only — no installs (compose ps, health, migration
#                  version, agent-runtime build, pi). Nonzero on failure.
#   --skip-agents  skip the agent-runtime phase (platform only)
#   -h, --help
#
# Out of scope: systemd units, prod profile (docker-compose.prod.yml strips
# ports — dashboard unreachable by design), real Jira/pi credentials, CA-issued
# TLS, distro support beyond apt/dnf.
# ==============================================================================
set -euo pipefail

# ---------- tunables (pin bump = one-line change) -----------------------------
PI_VER="${PI_VER:-1.4.0}"
GOOSE_VER="${GOOSE_VER:-latest}"   # or e.g. v3.24.1
DOCKER="${DOCKER:-docker}"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

SKIP_AGENTS=0
CHECK_ONLY=0
JIRA_CONFIGURED=0
for arg in "$@"; do
  case "$arg" in
    --skip-agents) SKIP_AGENTS=1 ;;
    --check)       CHECK_ONLY=1 ;;
    -h|--help)     sed -n '2,38p' "$0"; exit 0 ;;
    *) echo "unknown flag: $arg (try --help)" >&2; exit 2 ;;
  esac
done

SUDO=""
if [ "$(id -u)" -ne 0 ]; then SUDO="sudo"; fi

log()  { printf '\033[1;34m[setup]\033[0m %s\n' "$*"; }
ok()   { printf '\033[1;32m[ ok ]\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m[warn ]\033[0m %s\n' "$*" >&2; }
die()  { printf '\033[1;31m[FAIL ]\033[0m %s\n' "$*" >&2; exit 1; }

# ---------- env helpers ------------------------------------------------------
env_get() { grep -E "^$1=" .env 2>/dev/null | head -1 | cut -d= -f2-; }
env_set() {
  if grep -qE "^$1=" .env; then
    sed -i "s|^$1=.*|$1=$2|" .env
  else
    printf '%s=%s\n' "$1" "$2" >> .env
  fi
}

# ---------- phase 1: distro --------------------------------------------------
detect_pm() {
  if command -v apt-get >/dev/null 2>&1; then PM=apt
  elif command -v dnf >/dev/null 2>&1; then PM=dnf
  else die "unsupported distro — this script covers Debian/Ubuntu (apt) and Fedora (dnf) only"; fi
  ok "package manager: $PM"
}

# ---------- phase 2: base packages -------------------------------------------
install_base() {
  log "installing base packages"
  if [ "$PM" = apt ]; then
    $SUDO apt-get update -qq
    $SUDO apt-get install -y -qq git curl openssl ca-certificates gnupg unzip
  else
    $SUDO dnf install -y -q git curl openssl ca-certificates gnupg2 unzip
  fi
  ok "git curl openssl unzip ready"
}

# ---------- phase 3: docker --------------------------------------------------
install_docker() {
  if command -v docker >/dev/null 2>&1 && docker compose version >/dev/null 2>&1; then
    ok "docker + compose plugin already installed"
  else
    log "installing Docker Engine + compose plugin (official repo)"
    if [ "$PM" = apt ]; then
      $SUDO install -m 0755 -d /etc/apt/keyrings
      curl -fsSL https://download.docker.com/linux/ubuntu/gpg | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg 2>/dev/null \
        || curl -fsSL https://download.docker.com/linux/debian/gpg | $SUDO gpg --dearmor -o /etc/apt/keyrings/docker.gpg
      $SUDO chmod a+r /etc/apt/keyrings/docker.gpg
      . /etc/os-release
      DISTRO_ID="${ID}"; [ "$DISTRO_ID" = "ubuntu" ] || DISTRO_ID="debian"
      echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/$DISTRO_ID ${VERSION_CODENAME} stable" \
        | $SUDO tee /etc/apt/sources.list.d/docker.list >/dev/null
      $SUDO apt-get update -qq
      $SUDO apt-get install -y -qq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
    else
      $SUDO dnf -y -q install dnf-plugins-core
      $SUDO dnf config-manager --add-repo https://download.docker.com/linux/fedora/docker-ce.repo >/dev/null 2>&1 \
        || $SUDO dnf-3 config-manager --add-repo https://download.docker.com/linux/fedora/docker-ce.repo >/dev/null
      $SUDO dnf install -y -q docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin \
        || $SUDO dnf install -y -q docker docker-compose-plugin
    fi
  fi
  $SUDO systemctl enable --now docker >/dev/null 2>&1 || true

  # privilege: prefer group membership; fall back to sudo docker
  if docker info >/dev/null 2>&1; then
    DOCKER="docker"
  elif $SUDO docker info >/dev/null 2>&1; then
    if [ "$(id -u)" -ne 0 ]; then
      $SUDO usermod -aG docker "$USER" || true
      warn "added $USER to group docker — new shells get rootless docker; this run continues via sudo"
    fi
    DOCKER="$SUDO docker"
  else
    die "docker daemon not reachable"
  fi
  $DOCKER compose version >/dev/null || die "compose plugin missing"
  ok "docker: $($DOCKER version --format '{{.Server.Version}}' 2>/dev/null || echo ok)"
}

# ---------- phase 4: .env secrets --------------------------------------------
KNOWN_PLACEHOLDERS='vtx-dev-secret-token-change-in-production|you@example.com'
rand() { openssl rand -hex 32; }

gen_env() {
  [ -f .env ] || { cp .env.example .env; log ".env created from .env.example"; }
  local key val
  for key in API_AUTH_TOKEN GATEWAY_INTERNAL_AUTH_TOKEN WEBHOOK_PATH_SECRET_PRIMARY \
             REGISTRY_ACTOR_SCHEDULER_TOKEN REGISTRY_ACTOR_ORCHESTRATOR_TOKEN \
             REGISTRY_ACTOR_OPERATOR_TOKEN REGISTRY_ACTOR_POLICY_TOKEN; do
    val="$(env_get "$key")"
    if [ -z "$val" ] || echo "$val" | grep -qE "^($KNOWN_PLACEHOLDERS)$"; then
      env_set "$key" "$(rand)"
      log "generated $key"
    fi
  done
  # Jira creds can't be fabricated — placeholders let the api pass fail-closed
  # validation so the platform starts; real intake needs the manual step below.
  val="$(env_get JIRA_API_TOKEN)"
  if [ -z "$val" ]; then
    env_set JIRA_API_TOKEN "SETUP-PLACEHOLDER-$(rand)"
    warn "JIRA_API_TOKEN is a placeholder — real Jira intake will 401 until you set it"
  fi
  val="$(env_get JIRA_API_EMAIL)"
  if [ -z "$val" ] || [ "$val" = "you@example.com" ]; then
    env_set JIRA_API_EMAIL "setup-pending@example.invalid"
    warn "JIRA_API_EMAIL is a placeholder — set your real Jira account email"
  fi

  # backend Validate() requires every secret distinct
  local dup
  dup=$(for k in API_AUTH_TOKEN GATEWAY_INTERNAL_AUTH_TOKEN WEBHOOK_PATH_SECRET_PRIMARY \
                 REGISTRY_ACTOR_SCHEDULER_TOKEN REGISTRY_ACTOR_ORCHESTRATOR_TOKEN \
                 REGISTRY_ACTOR_OPERATOR_TOKEN REGISTRY_ACTOR_POLICY_TOKEN; do env_get "$k"; done \
        | sort | uniq -d | head -1)
  [ -z "$dup" ] || die "duplicate secret value in .env — backend fails closed on colliding tokens"
  ok ".env secrets distinct"
}

# ---------- phase 4b: jira intake (interactive prompt) ------------------------
is_placeholder() {
  [ -z "$1" ] || [[ "$1" == SETUP-PLACEHOLDER-* ]] || [[ "$1" == *example.invalid ]] || [ "$1" = "you@example.com" ]
}

write_jira_yaml() {
  local file="infra/compose/config/jira.local.yaml" project="$1" ids_csv="$2"
  {
    echo "# Generated by setup-vtx-host.sh — machine-local Jira intake policy."
    echo "# Mounted via JIRA_CONFIG_FILE=jira.local.yaml; committed jira.yaml stays the reference."
    echo "jira:"
    echo "  projects:"
    echo "    $project:"
    printf '      start_status_ids: ['
    local first=1 id
    IFS=',' read -ra _ids <<< "$ids_csv"
    for id in "${_ids[@]}"; do
      id="${id//[[:space:]]/}"
      [ -n "$id" ] || continue
      [ "$first" = 1 ] || printf ', '
      printf '"%s"' "$id"
      first=0
    done
    echo ']'
  } > "$file"
  env_set JIRA_CONFIG_FILE "jira.local.yaml"
  ok "jira intake policy -> $file ($project: $ids_csv)"
}

prompt_jira() {
  local email token
  email="$(env_get JIRA_API_EMAIL)"; token="$(env_get JIRA_API_TOKEN)"
  if ! is_placeholder "$token" && ! is_placeholder "$email"; then
    JIRA_CONFIGURED=1
    ok "jira credentials already configured — skipping prompts"
    return
  fi
  if [ ! -t 0 ]; then
    warn "non-interactive run — Jira placeholders kept; set JIRA_API_EMAIL/JIRA_API_TOKEN in .env and re-run"
    return
  fi

  local ans base site project ids
  echo
  ans=""; read -rp "Configure real Jira intake now? [Y/n] " ans || true
  case "${ans:-Y}" in [Nn]*) warn "skipped — Jira placeholders kept (backend starts, intake offline)"; return ;; esac

  base="$(env_get JIRA_BASE_URL)"; base="${base:-https://vtxsolution.atlassian.net}"
  ans=""; read -rp "Jira base URL [$base]: " ans || true; base="${ans:-$base}"
  site="$(env_get JIRA_SITE_ID)"
  site="${site:-$(echo "$base" | sed -E 's|^https?://||; s|/.*$||')}"
  ans=""; read -rp "Jira site ID [$site]: " ans || true; site="${ans:-$site}"
  ans=""; read -rp "Jira API email: " ans || true
  [ -n "$ans" ] || { warn "empty email — Jira placeholders kept"; return; }
  email="$ans"
  ans=""; read -rsp "Jira API token (input hidden): " ans || true; echo
  [ -n "$ans" ] || { warn "empty token — Jira placeholders kept"; return; }
  token="$ans"
  ans=""; read -rp "Jira project key [VTX]: " ans || true; project="${ans:-VTX}"
  ans=""; read -rp "Start status IDs for $project (comma-separated, Jira status — not transition — IDs) [10007]: " ans || true
  ids="${ans:-10007}"

  env_set JIRA_BASE_URL "$base"
  env_set JIRA_SITE_ID "$site"
  env_set JIRA_API_EMAIL "$email"
  env_set JIRA_API_TOKEN "$token"
  write_jira_yaml "$project" "$ids"
  JIRA_CONFIGURED=1
  ok "jira intake configured for $site / project $project"
}

# ---------- phase 5: gateway certs -------------------------------------------
gen_certs() {
  if [ -f infra/compose/nginx/certs/gateway.crt ]; then ok "gateway cert exists"; return; fi
  bash infra/compose/nginx/gen_certs.sh
  ok "gateway TLS cert generated (self-signed, dev)"
}

# ---------- phase 6: compose -------------------------------------------------
compose_up() {
  log "docker compose up -d (first build of backend/web images takes a while)"
  $DOCKER compose --env-file .env -f infra/compose/docker-compose.yml up -d
  ok "compose stack started"
}

wait_health() {
  local port tries=0
  port="$(env_get VTX_API_PORT)"; port="${port:-49080}"
  log "waiting for backend /health on :$port"
  until curl -sf "http://localhost:$port/health" >/dev/null 2>&1; do
    tries=$((tries+1))
    [ "$tries" -le 90 ] || die "backend /health did not come up — check: $DOCKER compose -f infra/compose/docker-compose.yml logs backend"
    sleep 2
  done
  ok "backend healthy"
}

# ---------- phase 7: goose + migrations --------------------------------------
install_goose() {
  if command -v goose >/dev/null 2>&1; then ok "goose present: $(command -v goose)"; return; fi
  local arch url
  case "$(uname -m)" in
    x86_64)  arch="x86_64" ;;
    aarch64|arm64) arch="arm64" ;;
    *) die "unsupported arch for goose binary: $(uname -m)" ;;
  esac
  if [ "$GOOSE_VER" = "latest" ]; then
    url="https://github.com/pressly/goose/releases/latest/download/goose_linux_${arch}"
  else
    url="https://github.com/pressly/goose/releases/download/${GOOSE_VER}/goose_linux_${arch}"
  fi
  mkdir -p "$HOME/.local/bin"
  curl -fsSL "$url" -o "$HOME/.local/bin/goose" && chmod +x "$HOME/.local/bin/goose"
  ok "goose → ~/.local/bin/goose"
}

migrate() {
  local pg_port db_url
  pg_port="$(env_get VTX_POSTGRES_PORT)"
  db_url="postgres://vtx_user:vtx_password@localhost:${pg_port:-49432}/vtx_platform?sslmode=disable"
  export PATH="$HOME/.local/bin:$PATH"
  # wait for postgres accept
  local tries=0
  until goose -dir apps/backend/db/migrations postgres "$db_url" status >/dev/null 2>&1; do
    tries=$((tries+1)); [ "$tries" -le 30 ] || die "postgres unreachable for migrations"
    sleep 2
  done
  goose -dir apps/backend/db/migrations postgres "$db_url" up
  # goose prints to stderr — merge streams so the version lands in the summary
  ok "migrations at $(goose -dir apps/backend/db/migrations postgres "$db_url" version 2>&1)"
}

# ---------- phase 8: agent runtime (host side) -------------------------------
install_agents() {
  log "installing fnm + Node (repo .node-version) + pnpm + pi"
  if ! command -v fnm >/dev/null 2>&1; then
    curl -fsSL https://fnm.vercel.app/install | bash -s -- --install-dir "$HOME/.local/share/fnm" --skip-shell
  fi
  export PATH="$HOME/.local/share/fnm:$HOME/.local/bin:$PATH"
  eval "$(fnm env)"
  fnm install "$(cat .node-version)"
  fnm default "$(cat .node-version)"
  fnm use "$(cat .node-version)"
  corepack enable
  # pnpm workspace: install at root, build the member via --filter
  pnpm install --frozen-lockfile
  pnpm --filter @vtx/agent-runtime build
  npm i -g "@earendil-works/pi-coding-agent@${PI_VER}"

  # PATH persistence for new shells (guarded, idempotent)
  for rc in "$HOME/.bashrc" "$HOME/.zshrc"; do
    [ -f "$rc" ] || continue
    grep -q 'vtx-setup PATH' "$rc" && continue
    cat >> "$rc" <<'RC'
# vtx-setup PATH (fnm node + user bins)
export PATH="$HOME/.local/bin:$HOME/.local/share/fnm:$PATH"
command -v fnm >/dev/null 2>&1 && eval "$(fnm env)"
RC
    log "PATH block appended to $rc"
  done
  ok "agent-runtime built; pi $(pi --version 2>/dev/null || echo '?')"
}

# ---------- check mode -------------------------------------------------------
check() {
  local rc=0 port
  command -v docker >/dev/null 2>&1 || { warn "docker missing"; rc=1; }
  $DOCKER compose --env-file .env -f infra/compose/docker-compose.yml ps 2>/dev/null || { warn "compose stack not up"; rc=1; }
  port="$(env_get VTX_API_PORT)"; port="${port:-49080}"
  curl -sf "http://localhost:$port/health" >/dev/null && ok "api /health" || { warn "api /health unreachable"; rc=1; }
  if command -v goose >/dev/null 2>&1; then
    local pg_port
    pg_port="$(env_get VTX_POSTGRES_PORT)"
    goose -dir apps/backend/db/migrations postgres \
      "postgres://vtx_user:vtx_password@localhost:${pg_port:-49432}/vtx_platform?sslmode=disable" version \
      || rc=1
  else warn "goose missing"; rc=1; fi
  [ "$SKIP_AGENTS" = 1 ] || {
    [ -d apps/agent-runtime/dist ] && ok "agent-runtime dist" || { warn "agent-runtime not built"; rc=1; }
    command -v pi >/dev/null 2>&1 && ok "pi $(pi --version)" || { warn "pi missing"; rc=1; }
  }
  local jf
  jf="$(env_get JIRA_CONFIG_FILE)"; jf="${jf:-jira.yaml}"
  [ -f "infra/compose/config/$jf" ] && ok "jira config: $jf" || { warn "jira config missing: infra/compose/config/$jf"; rc=1; }
  is_placeholder "$(env_get JIRA_API_TOKEN)" && warn "JIRA_API_TOKEN placeholder — real Jira intake offline" || true
  [ "$rc" = 0 ] && ok "check: all green" || die "check: failures above"
}

# ---------- main -------------------------------------------------------------
if [ "$CHECK_ONLY" = 1 ]; then check; exit 0; fi

log "VTX host setup — repo: $REPO_ROOT"
detect_pm
install_base
install_docker
gen_env
prompt_jira
gen_certs
compose_up
wait_health
install_goose
migrate
[ "$SKIP_AGENTS" = 1 ] || install_agents

port="$(env_get VTX_API_PORT)"; port="${port:-49080}"
web="$(env_get VTX_WEB_PORT)"; web="${web:-49030}"
gw="$(env_get VTX_GATEWAY_PORT)"; gw="${gw:-49443}"

if [ "$JIRA_CONFIGURED" = 1 ]; then
  JIRA_STEP="
   1. Jira intake: configured (infra/compose/config/jira.local.yaml).
      Expose the gateway publicly, then in Jira Admin -> System -> Webhooks
      register: https://<public-host>/webhooks/<WEBHOOK_PATH_SECRET_PRIMARY>/jira
      (event: Issue updated + changelog; JQL: project = <PROJECT>)."
else
  JIRA_STEP="
   1. Real Jira intake: set JIRA_API_EMAIL + JIRA_API_TOKEN in .env (or re-run
      this script interactively), then
      $DOCKER compose --env-file .env -f infra/compose/docker-compose.yml up -d backend"
fi
cat <<EOF

────────────────────────────────────────────────────────────────
 VTX platform is up.
   web      http://localhost:$web
   api      http://localhost:$port   (GET /health, /ready)
   gateway  https://localhost:$gw    (self-signed — browser warning expected)
   temporal http://localhost:$(env_get VTX_TEMPORAL_UI_PORT || echo 49234)

 Manual steps remaining:$JIRA_STEP
   2. Coding agents: run 'pi' once and /login each provider (opencode-go,
      deepseek, openai-codex, 9router key) — OAuth cannot be automated.
   3. NOTE: agent execution is not yet wired into the running platform
      (internal/agent.Spawn has no production caller) — the host-side
      runtime validates agents via the smoke test only.
   4. git auth for the private repo is a prerequisite of the clone step,
      not of this script.

 Re-run anytime:  $0          (idempotent)
 Verify only:     $0 --check
────────────────────────────────────────────────────────────────
EOF
