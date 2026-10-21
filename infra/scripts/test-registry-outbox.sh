#!/usr/bin/env bash
# ==============================================================================
# VTX-3 Registry Outbox E2E — TR-03/08/12/13/20/22/24 + secret hygiene
# Runs against the Compose stack (postgres + registry-worker). Assertions read
# DB row counts: "no duplicates" is a data claim, not an exit code.
#
# Usage:  ./infra/scripts/test-registry-outbox.sh
# Output: PASS/FAIL per case; tee to a log for evidence.
# ==============================================================================
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

ENV_FILE="${JT_ENV_FILE:-$REPO_ROOT/.env}"
set -a; source "$ENV_FILE"; set +a

COMPOSE=(docker compose --env-file "$ENV_FILE"
  -f infra/compose/docker-compose.yml
  -f infra/compose/docker-compose.e2e.yml)

PSQL=("${COMPOSE[@]}" exec -T postgres psql -U vtx_user -d vtx_platform -tAqc)
WORKER=registry-worker
SCALE_FILE=infra/compose/docker-compose.e2e-scale.yml

PASS=0; FAIL=0; FAILED_CASES=()
note() { echo "     note: $*"; }
ok()   { echo "PASS  $1 — $2"; PASS=$((PASS+1)); }
bad()  { echo "FAIL  $1 — $2"; FAIL=$((FAIL+1)); FAILED_CASES+=("$1"); }

# q <sql> — single value, whitespace-stripped. Fails loudly: a silent empty
# result would make every "wait until 0" loop spin forever.
q() {
  local out
  if ! out=$("${PSQL[@]}" "$1" 2>&1); then
    echo "SQL ERROR: $out" >&2
    return 1
  fi
  printf '%s' "$out" | tr -d '[:space:]'
}

RUN_ID="$(date +%s | tail -c 4)"

# seed_event <issue_id> [event_type] [payload_override]
# Inserts one execution_intent + one outbox event, exactly as VTX-2 does, and
# echoes the outbox_id.
seed_event() {
  local issue_id="$1" event_type="${2:-execution_intent.created.v1}" payload="${3:-}"
  "${PSQL[@]}" "
    WITH i AS (
      INSERT INTO execution_intents (source, issue_key, target_status_id, status)
      VALUES ('jira_webhook', 'VTX-${issue_id}', '10007', 'pending') RETURNING intent_id
    )
    INSERT INTO outbox_events (intent_id, event_type, payload)
    SELECT intent_id, '${event_type}',
      COALESCE(NULLIF('${payload}', '')::jsonb,
        json_build_object('event_id', gen_random_uuid()::text, 'intent_id', intent_id::text,
          'issue_id', '${issue_id}', 'issue_key', 'VTX-${issue_id}',
          'jira_site_id', 'vtxsolution.atlassian.net', 'changelog_id', '77${RUN_ID}',
          'target_status_id', '10007', 'correlation_id', 'corr-${issue_id}',
          'occurred_at', '2026-10-19T06:30:00Z')::jsonb)
    FROM i
    RETURNING outbox_id;" 2>/dev/null | tr -d '[:space:]'
}

runs_for_issue() { q "SELECT count(*) FROM task_runs r JOIN registry_tasks t ON t.id = r.task_id WHERE t.jira_issue_id = '$1'"; }
pending_events() { q "SELECT count(*) FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type = '$1'"; }

wait_for_drain() { # <timeout_s> <event_type>
  local timeout="$1" event_type="$2" waited=0
  while [ "$(pending_events "$event_type")" != "0" ] && [ "$waited" -lt "$timeout" ]; do
    sleep 1; waited=$((waited+1))
  done
  pending_events "$event_type"
}

echo "=== VTX-3 registry outbox harness (run $RUN_ID) ===================="
"${COMPOSE[@]}" up -d --build postgres >/dev/null 2>&1
for _ in $(seq 30); do
  q "SELECT 1" >/dev/null 2>&1 && break
  sleep 1
done

# The registry schema is owned by db/migrations; this harness must be runnable
# on a fresh stack, so it applies them exactly like CI does (local goose + the
# published postgres port).
PG_PORT=$("${COMPOSE[@]}" port postgres 5432 2>/dev/null | head -1 | awk -F: '{print $NF}' | tr -d '[:space:]')
DSN="postgres://vtx_user:vtx_password@localhost:${PG_PORT}/vtx_platform?sslmode=disable"
# `q` succeeds whenever psql answers, so the schema check is on the value, not
# on the exit status.
if [ "$(q "SELECT to_regclass('public.task_runs') IS NOT NULL")" != "t" ]; then
  if command -v goose >/dev/null 2>&1; then
    goose -dir apps/backend/db/migrations postgres "$DSN" up \
      || { echo "FATAL: goose up failed on $DSN"; exit 1; }
  else
    echo "FATAL: registry schema missing and goose is not installed"
    exit 1
  fi
fi
SCHEMA_OK=$(q "SELECT to_regclass('public.task_runs') IS NOT NULL")
[ "$SCHEMA_OK" = "t" ] || { echo "FATAL: task_runs still missing after migration"; exit 1; }

# --- TR-03: two replicas, one run per intent --------------------------------
echo "--- TR-03: 2 dispatcher replicas ----------------------------------"
"${COMPOSE[@]}" up -d --build "$WORKER" >/dev/null 2>&1
"${COMPOSE[@]}" up -d --scale "$WORKER"=2 "$WORKER" >/dev/null 2>&1
sleep 2
REPLICAS=$("${COMPOSE[@]}" ps "$WORKER" --format '{{.Name}}' 2>/dev/null | grep -c . )
TR03_ISSUES=()
for n in 1 2 3 4 5 6; do
  issue="93${RUN_ID}${n}"
  seed_event "$issue" >/dev/null
  TR03_ISSUES+=("$issue")
done
LEFT=$(wait_for_drain 30 "execution_intent.created.v1")
RUNDUP=0
for issue in "${TR03_ISSUES[@]}"; do
  [ "$(runs_for_issue "$issue")" = "1" ] || RUNDUP=1
done
if [ "$REPLICAS" -ge 2 ] && [ "$LEFT" = "0" ] && [ "$RUNDUP" = "0" ]; then
  ok "TR-03" "2 replicas drained 6 intents to exactly 1 run each ($REPLICAS replicas observed)"
else
  bad "TR-03" "replicas=$REPLICAS pending=$LEFT duplicate-runs=$RUNDUP"
fi

# --- TR-08/TR-20: SIGKILL mid-flight, replay must not duplicate -------------
echo "--- TR-08/TR-20: crash + replay -----------------------------------"
CRASH_ISSUES=()
for n in 1 2 3 4; do
  issue="94${RUN_ID}${n}"
  seed_event "$issue" >/dev/null
  CRASH_ISSUES+=("$issue")
done
"${COMPOSE[@]}" up -d --scale "$WORKER"=2 "$WORKER" >/dev/null 2>&1
for round in 1 2 3; do
  sleep 1
  "${COMPOSE[@]}" kill -s SIGKILL "$WORKER" >/dev/null 2>&1
  sleep 1
  "${COMPOSE[@]}" up -d --scale "$WORKER"=2 "$WORKER" >/dev/null 2>&1
done
LEFT=$(wait_for_drain 45 "execution_intent.created.v1")
DUP=0
for issue in "${CRASH_ISSUES[@]}"; do
  [ "$(runs_for_issue "$issue")" = "1" ] || DUP=1
done
AUDITDUP=$(q "SELECT count(*) FROM (
  SELECT run_id, version FROM task_run_events WHERE version = 0 GROUP BY run_id, version HAVING count(*) > 1
) d")
CREATEDDUP=$(q "SELECT count(*) FROM (
  SELECT intent_id FROM outbox_events WHERE event_type = 'task_run.created.v1'
  GROUP BY intent_id HAVING count(*) > 1
) d")
if [ "$LEFT" = "0" ] && [ "$DUP" = "0" ] && [ "$AUDITDUP" = "0" ] && [ "$CREATEDDUP" = "0" ]; then
  ok "TR-08" "3x SIGKILL cycles drained all events: no duplicate run/audit/outbound"
else
  bad "TR-08" "pending=$LEFT duplicate-run=$DUP duplicate-audit=$AUDITDUP duplicate-outbound=$CREATEDDUP"
fi

# --- TR-12/TR-22: poison payload -> DLQ with reason, then redrive -----------
echo "--- TR-12/TR-22: poison + DLQ + redrive ---------------------------"
POISON_ISSUE="95${RUN_ID}1"
seed_event "$POISON_ISSUE" "execution_intent.created.v1" '{"event_id":"not-a-uuid"}' >/dev/null
sleep 6
DLQ=$(q "SELECT count(*) FROM outbox_events WHERE dead_lettered_at IS NOT NULL AND event_type='execution_intent.created.v1' AND payload ? 'event_id' AND payload->>'issue_id' IS NULL")
REASON=$("${PSQL[@]}" "SELECT coalesce(dead_letter_reason,'') FROM outbox_events WHERE dead_lettered_at IS NOT NULL AND payload->>'issue_id' IS NULL LIMIT 1" 2>/dev/null | tr -d '\n')
if [ "${DLQ:-0}" -ge 1 ] && [ -n "$REASON" ]; then
  ok "TR-12" "poison payload dead-lettered with reason: $REASON"
else
  bad "TR-12" "dlq=$DLQ reason='$REASON'"
fi
# Redrive: clear the marker and let the dispatcher try again; the payload is
# still poison, so it must dead-letter again instead of looping forever.
"${PSQL[@]}" "UPDATE outbox_events SET dead_lettered_at=NULL, dead_letter_reason=NULL, attempt=0, next_attempt_at=NULL
  WHERE dead_lettered_at IS NOT NULL AND payload->>'issue_id' IS NULL;" >/dev/null
sleep 6
REDLQ=$(q "SELECT count(*) FROM outbox_events WHERE dead_lettered_at IS NOT NULL AND payload->>'issue_id' IS NULL")
if [ "${REDLQ:-0}" -ge 1 ]; then
  ok "TR-22" "redriven poison event dead-lettered again (no infinite retry loop)"
else
  bad "TR-22" "redriven event is not in the DLQ again (redlq=$REDLQ)"
fi

# --- TR-13: an unhandled event type is never claimed by this consumer ------
echo "--- TR-13: routing guard ------------------------------------------"
OTHER_ISSUE="96${RUN_ID}1"
# Scope the assertion to THIS row: the registry also publishes
# task_run.created.v1 itself, so a type-wide count would be meaningless.
FOREIGN_ID=$(seed_event "$OTHER_ISSUE" "task_run.created.v1")
sleep 6
FOREIGN_STATE=$(q "SELECT coalesce(claimed_by,'-')||'|'||(published_at IS NULL)::text||'|'||(dead_lettered_at IS NULL)::text FROM outbox_events WHERE outbox_id='${FOREIGN_ID}'")
if [ "$FOREIGN_STATE" = "-|true|true" ]; then
  ok "TR-13" "a task_run.created.v1 row is never claimed, published or dead-lettered by the registry consumer"
else
  bad "TR-13" "foreign event row state = '$FOREIGN_STATE', want '-|true|true'"
fi

# --- TR-24: backfill = enable the dispatcher, pending goes to 0 ------------
echo "--- TR-24: backfill ------------------------------------------------"
# A real backfill is "work accumulated while the consumer was down": stop the
# dispatcher, seed, measure, start it again.
"${COMPOSE[@]}" stop "$WORKER" >/dev/null 2>&1
for n in 1 2 3; do
  seed_event "97${RUN_ID}${n}" >/dev/null
done
PENDING_BEFORE=$(pending_events "execution_intent.created.v1")
"${COMPOSE[@]}" up -d "$WORKER" >/dev/null 2>&1
LEFT=$(wait_for_drain 30 "execution_intent.created.v1")
if [ "$LEFT" = "0" ] && [ "${PENDING_BEFORE:-0}" -ge 3 ]; then
  ok "TR-24" "backfill drained $PENDING_BEFORE pending intents to 0 with no extra job"
else
  bad "TR-24" "before=$PENDING_BEFORE after=$LEFT"
fi


# --- TR-07: 20 concurrent writers from 2 API replicas ----------------------
# AC-05/TR-07: the CAS must be enforced by PostgreSQL, not by an in-process
# mutex. Each replica is hit directly (the gateway only proxies /webhooks/).
echo "--- TR-07: 20 concurrent writers, 2 API replicas -------------------"
TR07_ISSUE="95${RUN_ID}"
seed_event "$TR07_ISSUE" >/dev/null
wait_for_drain 30 "execution_intent.created.v1" >/dev/null
TR07_RUN=$(q "SELECT r.run_id FROM task_runs r JOIN registry_tasks t ON t.id = r.task_id WHERE t.jira_issue_id = '${TR07_ISSUE}' LIMIT 1")
"${COMPOSE[@]}" up -d --build backend >/dev/null 2>&1
"${COMPOSE[@]}" -f "$SCALE_FILE" up -d --scale backend=2 backend >/dev/null 2>&1
sleep 5
mapfile -t TR07_NODES < <("${COMPOSE[@]}" -f "$SCALE_FILE" ps -q backend)
TR07_NODES_N=${#TR07_NODES[@]}
send_transition() { # <container> <outfile>
  docker exec "$1" wget -q -S -O /dev/null \
    --header="Authorization: Bearer ${REGISTRY_ACTOR_OPERATOR_TOKEN}" \
    --header="Content-Type: application/json" \
    --post-data='{"expected_version":0,"action":"cancel","reason":"concurrency"}' \
    "http://127.0.0.1:49080/api/v1/registry/runs/${TR07_RUN}/transitions" \
    >"$2" 2>&1 || true
}
rm -f /tmp/vtx_tr07_*.out
for i in $(seq 1 20); do
  send_transition "${TR07_NODES[$(( (i - 1) % TR07_NODES_N ))]}" "/tmp/vtx_tr07_$i.out" &
done
wait
TR07_OK=$(grep -l 'HTTP/1.1 200' /tmp/vtx_tr07_*.out | wc -l)
TR07_CONFLICT=$(grep -l 'HTTP/1.1 409' /tmp/vtx_tr07_*.out | wc -l)
TR07_VER=$(q "SELECT version FROM task_runs WHERE run_id='${TR07_RUN}'")
TR07_STATE=$(q "SELECT state FROM task_runs WHERE run_id='${TR07_RUN}'")
TR07_AUDIT=$(q "SELECT count(*) FROM task_run_events WHERE run_id='${TR07_RUN}' AND version=1")
if [ "$TR07_NODES_N" -ge 2 ] && [ "$TR07_RUN" != "" ] && [ "$TR07_OK" = "1" ] \
   && [ "$TR07_CONFLICT" = "19" ] && [ "$TR07_VER" = "1" ] && [ "$TR07_STATE" = "cancelled" ] \
   && [ "$TR07_AUDIT" = "1" ]; then
  ok "TR-07" "$TR07_NODES_N replicas, 20 writers -> 1x200 + 19x409, version 1, one audit row"
else
  bad "TR-07" "replicas=$TR07_NODES_N run=${TR07_RUN:-none} ok=$TR07_OK conflict=$TR07_CONFLICT version=$TR07_VER state=$TR07_STATE audit=$TR07_AUDIT"
fi
"${COMPOSE[@]}" -f "$SCALE_FILE" up -d --scale backend=1 backend >/dev/null 2>&1
sleep 3

# --- Secret hygiene: registry tokens never reach container logs ------------
echo "--- Secret hygiene ------------------------------------------------"
"${COMPOSE[@]}" logs --no-color --since 15m "$WORKER" backend > /tmp/vtx_registry_logs.txt 2>&1 || true
LEAK=0
for val in "${REGISTRY_ACTOR_SCHEDULER_TOKEN:-}" "${REGISTRY_ACTOR_ORCHESTRATOR_TOKEN:-}" \
           "${REGISTRY_ACTOR_OPERATOR_TOKEN:-}" "${REGISTRY_ACTOR_POLICY_TOKEN:-}"; do
  [ -n "$val" ] || continue
  if grep -qF "$val" /tmp/vtx_registry_logs.txt; then LEAK=1; echo "     LEAKED: $val"; fi
done
if [ "$LEAK" = "0" ]; then
  ok "SECRETS" "no registry actor token in container logs"
else
  bad "SECRETS" "registry actor token found in container logs"
fi

# --- operational metrics (Phase 6 contract: two queries, real numbers) -----
echo "--- Metrics --------------------------------------------------------"
echo "     active runs by state:"
"${PSQL[@]}" "SELECT state, count(*) FROM task_runs WHERE state NOT IN ('completed','failed','cancelled') GROUP BY state ORDER BY state;" 2>/dev/null
echo "     outbox lag (execution_intent.created.v1):"
"${PSQL[@]}" "SELECT count(*), min(created_at) FROM outbox_events WHERE published_at IS NULL AND dead_lettered_at IS NULL AND event_type='execution_intent.created.v1';" 2>/dev/null

echo "=== $PASS passed, $FAIL failed ====================================="
if [ "$FAIL" -gt 0 ]; then
  printf 'failed: %s\n' "${FAILED_CASES[*]}"
  exit 1
fi
