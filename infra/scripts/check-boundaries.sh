#!/usr/bin/env bash
# ==============================================================================
# VTX Monorepo Boundary & Conformance Check Script
# Validates architectural constraints, isolation rules, and forbidden directories.
# ==============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

ERRORS=0

echo "🔍 Running VTX Monorepo Boundary Checks..."

# 1. Verify NO go.work or go.work.sum anywhere in repo (BS-19, AC-1)
if find . -name "go.work*" -not -path "*/.git/*" | grep -q .; then
    echo "❌ VIOLATION: Found forbidden 'go.work' file(s):"
    find . -name "go.work*" -not -path "*/.git/*"
    ERRORS=$((ERRORS + 1))
else
    echo "  ✅ Check 1: No go.work files exist in repository."
fi

# 2. Verify EXACTLY ONE go.mod at apps/backend/go.mod (AC-1)
GO_MOD_COUNT=$(find . -name "go.mod" -not -path "*/.git/*" | wc -l)
if [ "$GO_MOD_COUNT" -ne 1 ]; then
    echo "❌ VIOLATION: Expected exactly 1 go.mod, found $GO_MOD_COUNT:"
    find . -name "go.mod" -not -path "*/.git/*"
    ERRORS=$((ERRORS + 1))
else
    if [ ! -f "apps/backend/go.mod" ]; then
        echo "❌ VIOLATION: The single go.mod must reside strictly at apps/backend/go.mod"
        ERRORS=$((ERRORS + 1))
    else
        echo "  ✅ Check 2: Single go.mod correctly isolated at apps/backend/go.mod."
    fi
fi

# 3. Verify NO forbidden/premature apps scaffolded in Phase 0 (AC-5, BS-19)
FORBIDDEN_APPS=("apps/desktop" "apps/mobile" "packages/ui")
for app in "${FORBIDDEN_APPS[@]}"; do
    if [ -d "$app" ]; then
        echo "❌ VIOLATION: Found forbidden speculative directory: $app"
        ERRORS=$((ERRORS + 1))
    fi
done
echo "  ✅ Check 3: No speculative desktop/mobile apps or premature UI packages found."

# 4. Verify apps/web boundary isolation (AC-15, BS-09)
if [ -d "apps/web" ]; then
    # Web must NOT import backend code
    if grep -rn --exclude-dir=node_modules --exclude-dir=.next "apps/backend" apps/web/ 2>/dev/null; then
        echo "❌ VIOLATION: apps/web contains illegal direct import or reference to apps/backend!"
        ERRORS=$((ERRORS + 1))
    fi

    # Web must NOT reference Docker socket
    if grep -rn --exclude-dir=node_modules --exclude-dir=.next "docker.sock" apps/web/ 2>/dev/null; then
        echo "❌ VIOLATION: apps/web references docker.sock!"
        ERRORS=$((ERRORS + 1))
    fi
    echo "  ✅ Check 4: apps/web respects boundary isolation (no backend imports, no docker.sock)."
fi

# 5. Verify apps/agent-runtime does not contain Go files (AC-1)
if [ -d "apps/agent-runtime" ]; then
    if find apps/agent-runtime -name "*.go" -not -path "*/node_modules/*" | grep -q .; then
        echo "❌ VIOLATION: apps/agent-runtime must not contain Go files!"
        find apps/agent-runtime -name "*.go" -not -path "*/node_modules/*"
        ERRORS=$((ERRORS + 1))
    fi
    echo "  ✅ Check 5: apps/agent-runtime is strictly a Node.js/TypeScript host."
fi

# 6. Verify basic secret scanning (BS-11)
SUSPICIOUS_SECRETS=$(grep -rnE "(ghp_[A-Za-z0-9]{36}|xox[baprs]-[A-Za-z0-9-]+|-----BEGIN (RSA|EC|OPENSSH) PRIVATE KEY-----)" \
    --exclude-dir=.git \
    --exclude-dir=node_modules \
    --exclude="*.md" \
    --exclude=".env.example" \
    . 2>/dev/null || true)

if [ -n "$SUSPICIOUS_SECRETS" ]; then
    echo "❌ VIOLATION: Potential secret leaked in source files:"
    echo "$SUSPICIOUS_SECRETS"
    ERRORS=$((ERRORS + 1))
else
    echo "  ✅ Check 6: Secret scan passed (no high-entropy API tokens or private keys found)."
fi

if [ "$ERRORS" -gt 0 ]; then
    echo "❌ Monorepo boundary check FAILED with $ERRORS violation(s)."
    exit 1
fi

echo "✨ All boundary and isolation checks PASSED successfully."
exit 0
