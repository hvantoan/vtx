#!/usr/bin/env bash
# wait-for-it.sh: wait for host:port to become available
set -euo pipefail

HOST="${1:-localhost}"
PORT="${2:-5432}"
TIMEOUT="${3:-30}"

echo "⏳ Waiting for ${HOST}:${PORT} (timeout: ${TIMEOUT}s)..."

for i in $(seq 1 "$TIMEOUT"); do
    if nc -z "$HOST" "$PORT" 2>/dev/null || (exec 3<>/dev/tcp/"$HOST"/"$PORT") 2>/dev/null; then
        echo "✅ ${HOST}:${PORT} is available."
        exit 0
    fi
    sleep 1
done

echo "❌ Timeout reached waiting for ${HOST}:${PORT}."
exit 1
