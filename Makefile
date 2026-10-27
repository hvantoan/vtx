# ==============================================================================
# VTX Monorepo Makefile
# Sole entrypoint for building, testing, linting, and running services.
# ==============================================================================

SHELL := /usr/bin/env bash
export GOWORK := off

BACKEND_DIR := apps/backend
BIN_DIR := $(BACKEND_DIR)/bin

.PHONY: all help doctor dev build build-backend build-web build-agent test test-backend test-web test-agent test-boundaries lint vet typecheck migrate-up migrate-down compose-up compose-down compose-logs master master-once orchestrate orchestrate-once user-password clean

all: build

help:
	@echo "VTX Monorepo Developer Commands:"
	@echo "  make doctor            - Check local developer environment and toolchain"
	@echo "  make dev               - Run local development environment"
	@echo "  make build             - Build all apps (backend, web, agent-runtime)"
	@echo "  make build-backend     - Compile Go binaries (api, worker, cli)"
	@echo "  make build-web         - Build Next.js application"
	@echo "  make build-agent       - Compile Node.js agent runtime"
	@echo "  make test              - Run all test suites across the monorepo"
	@echo "  make test-backend      - Run Go tests with race detector"
	@echo "  make test-web          - Run Next.js frontend tests"
	@echo "  make test-agent        - Run Agent runtime tests"
	@echo "  make test-boundaries   - Run monorepo architectural boundary checks"
	@echo "  make lint              - Lint all projects"
	@echo "  make vet               - Run go vet on backend"
	@echo "  make typecheck         - Typecheck TypeScript projects"
	@echo "  make compose-up        - Start PostgreSQL and Temporal with Docker Compose"
	@echo "  make compose-down      - Stop Docker Compose services"
	@echo "  make master            - Run the Jira master poller loop (30 min interval)"
	@echo "  make master-once       - Poll Jira once and exit"
	@echo "  make orchestrate       - Run the coding-stage orchestrator launcher loop"
	@echo "  make orchestrate-once  - Launch eligible coding runs once and wait"
	@echo "  make user-password     - Set/reset the operator login password"
	@echo "  make clean             - Remove build artifacts and temporary files"

# ------------------------------------------------------------------------------
# Diagnostics & Doctor
# ------------------------------------------------------------------------------
doctor:
	@go -C $(BACKEND_DIR) run ./cmd/cli doctor

# ------------------------------------------------------------------------------
# Build Targets
# ------------------------------------------------------------------------------
build: build-backend build-web build-agent

build-backend:
	@mkdir -p $(BIN_DIR)
	@echo "📦 Building Go binaries..."
	@go -C $(BACKEND_DIR) build -o bin/api ./cmd/api
	@go -C $(BACKEND_DIR) build -o bin/worker ./cmd/worker
	@go -C $(BACKEND_DIR) build -o bin/platform ./cmd/cli
	@echo "✅ Backend binaries built in $(BIN_DIR)"

build-web:
	@echo "📦 Building Web application..."
	@pnpm --filter @vtx/web build

build-agent:
	@echo "📦 Building Agent runtime..."
	@pnpm --filter @vtx/agent-runtime build

# ------------------------------------------------------------------------------
# Test Targets
# ------------------------------------------------------------------------------
test: test-boundaries test-backend test-agent test-web

test-backend:
	@echo "🧪 Running Go unit tests..."
	@go -C $(BACKEND_DIR) test -v -race ./...

# Integration tests own the database they are pointed at (goose up/down/up +
# seeded VTX-2 rows) — always pass a throwaway DSN.
test-integration:
	@test -n "$(TEST_DATABASE_URL)" || { echo "❌ TEST_DATABASE_URL is required (throwaway DB)"; exit 1; }
	@echo "🧪 Running Go integration tests against real Postgres..."
	@TEST_DATABASE_URL="$(TEST_DATABASE_URL)" go -C $(BACKEND_DIR) test -tags integration -race -p 1 ./...

test-agent:
	@echo "🧪 Running Agent runtime tests..."
	@pnpm --filter @vtx/agent-runtime test

test-web:
	@echo "🧪 Running Web tests..."
	@pnpm --filter @vtx/web test

test-boundaries:
	@./infra/scripts/check-boundaries.sh

# ------------------------------------------------------------------------------
# Lint & Typecheck
# ------------------------------------------------------------------------------
lint: vet lint-web lint-agent

vet:
	@echo "🔍 Running go vet..."
	@go -C $(BACKEND_DIR) vet ./...

lint-web:
	@pnpm --filter @vtx/web lint

lint-agent:
	@pnpm --filter @vtx/agent-runtime lint

typecheck: typecheck-web typecheck-agent

typecheck-web:
	@pnpm --filter @vtx/web typecheck

typecheck-agent:
	@pnpm --filter @vtx/agent-runtime typecheck

# ------------------------------------------------------------------------------
# Database Migrations (Goose)
# ------------------------------------------------------------------------------
migrate-up:
	@goose -dir $(BACKEND_DIR)/db/migrations postgres "$${DATABASE_URL:-postgres://vtx_user:vtx_password@localhost:49432/vtx_platform?sslmode=disable}" up

migrate-down:
	@goose -dir $(BACKEND_DIR)/db/migrations postgres "$${DATABASE_URL:-postgres://vtx_user:vtx_password@localhost:49432/vtx_platform?sslmode=disable}" down

migrate-status:
	@goose -dir $(BACKEND_DIR)/db/migrations postgres "$${DATABASE_URL:-postgres://vtx_user:vtx_password@localhost:49432/vtx_platform?sslmode=disable}" status

# ------------------------------------------------------------------------------
# Docker Compose Management
# ------------------------------------------------------------------------------
COMPOSE_FILE := infra/compose/docker-compose.yml
# docker compose -f <path> does not auto-load the repo-root .env — pass it
# explicitly so ${JIRA_*} / ${GATEWAY_*} / ${WEBHOOK_*} interpolation resolves.
COMPOSE_ENV := $(if $(wildcard .env),--env-file .env,)

compose-up:
	@docker compose $(COMPOSE_ENV) -f $(COMPOSE_FILE) up -d

compose-down:
	@docker compose $(COMPOSE_ENV) -f $(COMPOSE_FILE) down

compose-logs:
	@docker compose $(COMPOSE_ENV) -f $(COMPOSE_FILE) logs -f

# ------------------------------------------------------------------------------
# Jira master poller (MCP, deterministic 30-minute loop)
# ------------------------------------------------------------------------------
master:
	@pnpm --filter @vtx/agent-runtime master

master-once:
	@pnpm --filter @vtx/agent-runtime master:once

# ------------------------------------------------------------------------------
# Coding-stage orchestrator launcher (Pi headless per coding run)
# ------------------------------------------------------------------------------
orchestrate:
	@go -C $(BACKEND_DIR) run ./cmd/worker orchestrate

orchestrate-once:
	@go -C $(BACKEND_DIR) run ./cmd/worker orchestrate --once

# ------------------------------------------------------------------------------
# Operator login (single account; prompts twice when stdin is a TTY)
# ------------------------------------------------------------------------------
user-password:
	@docker compose $(COMPOSE_ENV) -f $(COMPOSE_FILE) exec backend \
		/app/worker user set-password

# ------------------------------------------------------------------------------
# Cleanup
# ------------------------------------------------------------------------------
clean:
	@rm -rf $(BIN_DIR)
	@rm -rf apps/backend/coverage.out
	@rm -rf apps/web/.next apps/web/out
	@rm -rf apps/agent-runtime/dist
	@echo "🧹 Clean completed."
