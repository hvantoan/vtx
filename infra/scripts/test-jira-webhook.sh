#!/usr/bin/env bash
# ==============================================================================
# VTX-2 Jira Webhook Intake E2E — JT-01..JT-18 (SPEC-AUTO-01)
# Runs against the Compose stack + jira-stub (mock Jira REST API).
# Every dedup assertion checks DB row counts, not just HTTP responses.
#
# Usage:  ./infra/scripts/test-jira-webhook.sh
# Output: PASS/FAIL per case; evidence written to stdout (tee to the log).
# ==============================================================================
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

ENV_FILE="$REPO_ROOT/.env"
set -a; source "$ENV_FILE"; set +a

COMPOSE=(docker compose --env-file "$ENV_FILE"
  -f infra/compose/docker-compose.yml
  -f infra/compose/docker-compose.e2e.yml)
SCALE_FILE=infra/compose/docker-compose.e2e-scale.yml

GATEWAY="https://localhost:${VTX_GATEWAY_PORT:-49443}"
SECRET="${WEBHOOK_PATH_SECRET_PRIMARY:?missing in .env}"
SECONDARY="secondaryrot9e2b4a1c8d7f3e6b5a9c0d2e4f6a8b1c3d5e7f9a0b"
GWT="${GATEWAY_INTERNAL_AUTH_TOKEN:?missing in .env}"
PSQL=(docker exec vtx-postgres psql -U vtx_user -d vtx_platform -tAc)

PASS=0; FAIL=0; FAILED_CASES=()

note() { echo "     note: $*"; }
ok()   { echo "PASS  $1 — $2"; PASS=$((PASS+1)); }
bad()  { echo "FAIL  $1 — $2"; FAIL=$((FAIL+1)); FAILED_CASES+=("$1"); }

RUN_ID="$(date +%s | tail -c 4)"   # stub 503-once state persists across runs
ISSUE_503="190503${RUN_ID}"

expect_status() { # name desc expected actual
  if [ "$3" = "$4" ]; then ok "$1" "$2 (HTTP $4)"; else bad "$1" "$2 (want $3, got $4)"; fi
}

# send_issue <issue_id> <issue_key> <changelog_id> <status_to> [secret] [project]
send_issue() {
  local secret="${5:-$SECRET}"
  local project="${6:-VTX}"
  curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "$GATEWAY/webhooks/$secret/jira" \
    -H 'Content-Type: application/json' \
    -d "$(cat <<JSON
{"timestamp":1760500000000,"webhookEvent":"jira:issue_updated",
 "issue":{"id":"$1","key":"$2","self":"https://vtxsolution.atlassian.net/rest/api/3/issue/$1",
          "fields":{"project":{"key":"$project"},"status":{"id":"$4","name":"x"}}},
 "changelog":{"id":"$3","items":[{"field":"assignee","to":"u"},
                                 {"field":"status","fieldId":"status","from":"10000","to":"$4"}]}}
JSON
)"
}

send_raw() { # <secret> <content-type> <body>
  curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "$GATEWAY/webhooks/$1/jira" -H "Content-Type: $2" --data-binary "$3"
}

count_inbox()    { "${PSQL[@]}" "SELECT count(*) FROM jira_webhook_inbox WHERE changelog_id='$1'"; }
count_intents()  { "${PSQL[@]}" "SELECT count(*) FROM execution_intents WHERE issue_key='$1'"; }
count_outbox()   { "${PSQL[@]}" "SELECT count(*) FROM outbox_events WHERE payload->>'issue_key'='$1'"; }
inbox_outcome()  { "${PSQL[@]}" "SELECT outcome FROM jira_webhook_inbox WHERE changelog_id='$1' LIMIT 1"; }

cleanup() {
  "${PSQL[@]}" "DELETE FROM outbox_events" >/dev/null
  "${PSQL[@]}" "DELETE FROM jira_webhook_inbox" >/dev/null
  "${PSQL[@]}" "DELETE FROM execution_intents" >/dev/null
}

wait_http() { # <url> <tries>
  for _ in $(seq "$2"); do
    if curl -sk -o /dev/null -w '%{http_code}' "$1" | grep -qE '200|401|403|404'; then return 0; fi
    sleep 1
  done
  return 1
}

echo "==================================================================="
echo " Jira Webhook Intake E2E — $(date -u '+%Y-%m-%dT%H:%M:%SZ')"
echo " Stack: ${COMPOSE[*]} --profile e2e"
echo "==================================================================="

# --- Stack up: postgres, backend (rebuilt), gateway, jira-stub ---------------
echo "[setup] building + starting single-replica e2e stack"
"${COMPOSE[@]}" --profile e2e up -d --build backend gateway jira-stub postgres >/dev/null 2>&1
wait_http "$GATEWAY/" 60 || { echo "FATAL: gateway unreachable"; exit 2; }
wait_http "http://localhost:${VTX_API_PORT:-49080}/health" 60 || { echo "FATAL: backend unreachable"; exit 2; }
sleep 3
cleanup

echo "--- Layer 1: gateway auth (JT-02) ---------------------------------"
S=$(send_raw "definitely-wrong-secret" application/json '{}')
expect_status "JT-02" "wrong path secret -> 403 from nginx" "403" "$S"

echo "--- Layer 2: internal bearer token (JT-04, JT-05) ------------------"
# Reach backend directly (published port 49080) to test layer 2 isolation.
S=$(curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "http://localhost:${VTX_API_PORT:-49080}/api/v1/integrations/jira/webhooks" \
    -H 'Content-Type: application/json' -d '{}')
expect_status "JT-04" "no Authorization header -> 401" "401" "$S"

S=$(curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "http://localhost:${VTX_API_PORT:-49080}/api/v1/integrations/jira/webhooks" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer ${API_AUTH_TOKEN:-vtx-dev-secret-token-change-in-production}" -d '{}')
expect_status "JT-05" "public API_AUTH_TOKEN rejected on webhook route -> 401" "401" "$S"

echo "--- Ingress controls (JT-06, JT-07) -------------------------------"
# Forge a gateway-style request: correct path secret + injected bearer.
S=$(curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "http://localhost:${VTX_API_PORT:-49080}/api/v1/integrations/jira/webhooks" \
    -H 'Content-Type: text/plain' \
    -H "Authorization: Bearer $GWT" -d '{}')
expect_status "JT-06" "text/plain Content-Type -> 400" "400" "$S"

python3 - <<'PY' > /tmp/jt_big.json
print('{"a":"' + 'x' * (280 * 1024) + '"}')
PY
S=$(curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "http://localhost:${VTX_API_PORT:-49080}/api/v1/integrations/jira/webhooks" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $GWT" --data-binary @/tmp/jt_big.json)
expect_status "JT-07" "payload >256 KiB -> 413" "413" "$S"

echo "--- Ignorable traffic: 202, never penalized (JT-08, JT-15) ---------"
S=$(curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "http://localhost:${VTX_API_PORT:-49080}/api/v1/integrations/jira/webhooks" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $GWT" \
    -d '{"webhookEvent":"jira:issue_updated","issue":{"id":"190001","key":"VTX-91","self":"x",
        "fields":{"project":{"key":"VTX"},"status":{"id":"10000"}}},
        "changelog":{"id":"cl-nonstatus","items":[{"field":"comment","to":null}]}}')
BODY=$(cat /tmp/jt_resp.json)
if [ "$S" = "202" ] && echo "$BODY" | grep -q ignored_scope; then
  ok "JT-08" "non-status issue_updated -> 202 ignored_scope"
else
  bad "JT-08" "non-status issue_updated -> want 202 ignored_scope, got $S $BODY"
fi

S=$(curl -sk -o /tmp/jt_resp.json -w '%{http_code}' -X POST \
    "http://localhost:${VTX_API_PORT:-49080}/api/v1/integrations/jira/webhooks" \
    -H 'Content-Type: application/json' \
    -H "Authorization: Bearer $GWT" \
    -d '{"webhookEvent":"jira:issue_updated","issue":{"id":"190002","key":"VTX-92","self":"x",
        "fields":{"project":{"key":"VTX"},"status":{"id":"10000"}}}}')
BODY=$(cat /tmp/jt_resp.json)
if [ "$S" = "202" ]; then
  ok "JT-15" "missing changelog.id -> 202 (webhook-health policy overrides 422)"
else
  bad "JT-15" "missing changelog.id -> want 202, got $S $BODY"
fi

echo "--- Accepted path + idempotent duplicate (JT-01, JT-09) ------------"
S=$(send_issue 190001 VTX-101 cl-accept-1 10007)
BODY=$(cat /tmp/jt_resp.json)
INTENT1=$(echo "$BODY" | grep -oE '"intent_id":"[^"]+"' | cut -d'"' -f4)
if [ "$S" = "202" ] && [ -n "$INTENT1" ] \
   && [ "$(count_inbox cl-accept-1)" = "1" ] \
   && [ "$(count_intents VTX-101)" = "1" ] \
   && [ "$(count_outbox VTX-101)" = "1" ]; then
  ok "JT-01" "accepted -> 202, 1 inbox + 1 intent + 1 outbox"
else
  bad "JT-01" "accepted path -> got $S $BODY rows: inbox=$(count_inbox cl-accept-1) intent=$(count_intents VTX-101) outbox=$(count_outbox VTX-101)"
fi

S=$(send_issue 190001 VTX-101 cl-accept-1 10007)
BODY=$(cat /tmp/jt_resp.json)
INTENT2=$(echo "$BODY" | grep -oE '"intent_id":"[^"]+"' | cut -d'"' -f4)
if [ "$S" = "200" ] && [ "$INTENT2" = "$INTENT1" ] \
   && [ "$(count_intents VTX-101)" = "1" ]; then
  ok "JT-09" "duplicate redelivery -> 200 same intent_id, still 1 intent"
else
  bad "JT-09" "duplicate -> got $S $BODY (intent=$INTENT2 want $INTENT1)"
fi

# Re-entry semantics must run BEFORE the concurrency cleanup — it asserts a
# second intent accumulates for the same issue under a new changelog_id.
S=$(send_issue 190001 VTX-101 cl-accept-2 10007)
if [ "$S" = "202" ] && [ "$(count_intents VTX-101)" = "2" ]; then
  ok "JT-14" "re-entry (new changelog_id) -> NEW intent created"
else
  bad "JT-14" "re-entry -> got $S intents=$(count_intents VTX-101)"
fi

echo "--- Concurrency (JT-03a single replica, JT-03b two replicas) -------"
# Single replica, 8 parallel deliveries of a NEW transition key.
cleanup
for i in $(seq 1 8); do send_issue 190002 VTX-102 cl-conc-1 10007 >"/tmp/jt_par_$i" & done
wait
ACC=$(grep -l 202 /tmp/jt_par_* | wc -l); DUP=$(grep -l 200 /tmp/jt_par_* | wc -l)
if [ "$(count_intents VTX-102)" = "1" ] && [ "$ACC" = "1" ] && [ "$DUP" = "7" ]; then
  ok "JT-03a" "8 parallel deliveries, 1 replica -> 1 intent (1x202 + 7x200)"
else
  bad "JT-03a" "parallel dedup -> intents=$(count_intents VTX-102) acc=$ACC dup=$DUP"
fi

# Two replicas — proves dedup is DB-enforced, not an in-process mutex.
cleanup
"${COMPOSE[@]}" -f "$SCALE_FILE" --profile e2e up -d --scale backend=2 >/dev/null 2>&1
sleep 4
REPLICAS=$(docker compose --env-file "$ENV_FILE" -f infra/compose/docker-compose.yml -f infra/compose/docker-compose.e2e.yml -f "$SCALE_FILE" --profile e2e ps -q backend | wc -l)
note "backend replicas: $REPLICAS"
for i in $(seq 1 8); do send_issue 190003 VTX-103 cl-conc-2 10007 >"/tmp/jt_par2_$i" & done
wait
ACC=$(grep -l 202 /tmp/jt_par2_* | wc -l); DUP=$(grep -l 200 /tmp/jt_par2_* | wc -l)
if [ "$REPLICAS" -ge 2 ] && [ "$(count_intents VTX-103)" = "1" ] && [ "$ACC" = "1" ] && [ "$DUP" = "7" ]; then
  ok "JT-03b" "8 parallel deliveries, $REPLICAS replicas -> 1 intent"
else
  bad "JT-03b" "scaled dedup -> replicas=$REPLICAS intents=$(count_intents VTX-103) acc=$ACC dup=$DUP"
fi
"${COMPOSE[@]}" -f "$SCALE_FILE" --profile e2e up -d --scale backend=1 >/dev/null 2>&1
sleep 4
cleanup

echo "--- Status verification & scope (JT-10, JT-16) --------------------"
S=$(send_issue 190101 VTX-104 cl-stale-1 10007)   # stub: current status 10005
if [ "$S" = "202" ] && [ "$(inbox_outcome cl-stale-1)" = "ignored_stale" ] \
   && [ "$(count_intents VTX-104)" = "0" ]; then
  ok "JT-10" "stale transition (issue left Ready to Dev) -> ignored_stale, no intent"
else
  bad "JT-10" "stale -> got $S outcome=$(inbox_outcome cl-stale-1)"
fi

S=$(send_issue 190004 VTX-105 cl-scope-1 10007 "$SECRET" OTHER)
if [ "$S" = "202" ] && [ "$(inbox_outcome cl-scope-1)" = "ignored_scope" ] \
   && [ "$(count_intents VTX-105)" = "0" ]; then
  ok "JT-16" "project outside allowlist -> ignored_scope, no intent"
else
  bad "JT-16" "scope -> got $S outcome=$(inbox_outcome cl-scope-1)"
fi


echo "--- Retryable failures: 503, dedup key unclaimed (JT-11..JT-13) -----"
S=$(send_issue 1904290 VTX-111 cl-429 10007)
ROWS=$(count_inbox cl-429)
if [ "$S" = "503" ] && [ "$ROWS" = "0" ]; then
  ok "JT-11" "Jira 429 -> 503, zero inbox rows (key unclaimed)"
else
  bad "JT-11" "429 -> got $S rows=$ROWS"
fi

S=$(send_issue 1905000 VTX-112 cl-500 10007)
ROWS=$(count_inbox cl-500)
if [ "$S" = "503" ] && [ "$ROWS" = "0" ]; then
  ok "JT-12" "Jira 500 -> 503, zero inbox rows"
else
  bad "JT-12" "500 -> got $S rows=$ROWS"
fi

S=$(send_issue "$ISSUE_503" VTX-113 "cl-503r-${RUN_ID}" 10007)
ROWS=$(count_inbox "cl-503r-${RUN_ID}")
S2=$(send_issue "$ISSUE_503" VTX-113 "cl-503r-${RUN_ID}" 10007)   # stub now answers 200/10007
if [ "$S" = "503" ] && [ "$S2" = "202" ] && [ "$(count_intents VTX-113)" = "1" ]; then
  ok "JT-13" "503 then Jira redelivery -> accepted (recovery path works)"
else
  bad "JT-13" "retry-then-accept -> got $S/$S2 intents=$(count_intents VTX-113)"
fi

echo "--- Secret hygiene (JT-17) ----------------------------------------"
"${COMPOSE[@]}" --profile e2e logs --no-color > /tmp/jt_all_logs.txt 2>&1 || true
LEAK=0
for val in "$SECRET" "$GWT"; do
  if grep -qF "$val" /tmp/jt_all_logs.txt; then LEAK=1; echo "     LEAKED: $val"; fi
done
if docker compose --env-file "$ENV_FILE" -f infra/compose/docker-compose.yml -f infra/compose/docker-compose.e2e.yml --profile e2e logs --no-color backend 2>/dev/null | grep -q '/webhooks/'; then
  LEAK=1; echo "     backend log contains /webhooks/ path"
fi
if [ "$LEAK" = "0" ]; then
  ok "JT-17" "zero secret values and zero /webhooks/ paths in container logs"
else
  bad "JT-17" "secret/path leakage detected in logs"
fi

echo "--- Rotation overlap (JT-18) ---------------------------------------"
cleanup
# Secondary inactive -> rejected.
S=$(send_issue 190005 VTX-118 cl-rot-1 10007 "$SECONDARY")
if [ "$S" != "403" ]; then bad "JT-18" "inactive secondary secret -> want 403, got $S"; fi
# Deploy gateway accepting BOTH (overlap), then retire primary.
export WEBHOOK_PATH_SECRET_SECONDARY="$SECONDARY"
"${COMPOSE[@]}" --profile e2e up -d gateway >/dev/null 2>&1
sleep 3
S2=$(send_issue 190005 VTX-118 cl-rot-2 10007 "$SECONDARY")
S3=$(send_issue 190006 VTX-118b cl-rot-3 10007 "$SECRET")
if [ "$S" = "403" ] && [ "$S2" = "202" ] && [ "$S3" = "202" ]; then
  ok "JT-18" "overlap: secondary activated + primary still accepted (no delivery loss)"
else
  bad "JT-18" "rotation -> inactive=$S secondary=$S2 primary=$S3"
fi
unset WEBHOOK_PATH_SECRET_SECONDARY
"${COMPOSE[@]}" --profile e2e up -d gateway >/dev/null 2>&1

echo "==================================================================="
echo " RESULT: $PASS PASS / $FAIL FAIL  (of $((PASS+FAIL)))"
if [ "$FAIL" -gt 0 ]; then
  echo " FAILED: ${FAILED_CASES[*]}"
  exit 1
fi
echo " ALL CHECKS PASSED"
exit 0
