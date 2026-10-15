---
title: VTX-2 review-fixes plan
date: 2026-10-15
summary: "Advised and planned the fix path for VTX-2 Jira webhook intake: review comment 10014 BLOCKED implementation d2326d4 with 5 P1 + 2 P2 findings. User-confirmed design decisions baked into plans/261015-0431-vtx-2-jira-webhook-review-fixes: per-project start_status_ids via YAML config with dual-control error (JIRA_CONFIG_PATH vs legacy env), StatusChecker→IssueFetcher returning authoritative identity+project+status, transition target gate before dedup, typed execution_intent.created.v1 payload using changelog_id naming and occurred_at from webhook timestamp with received_at fallback, docker-compose.prod.yml overlay removing public backend port (nginx already injects gateway token + masks secret), append-only webhook_receipts table for every authenticated request (unauthenticated never writes) with 7d retention, and CI evidence running migrations/sqlc-diff/JT-01..18 at >=20 concurrent across 2 instances plus tenant Admin Webhook verification."
---

# VTX-2 review-fixes plan

Advised and planned the fix path for VTX-2 Jira webhook intake: review comment 10014 BLOCKED implementation d2326d4 with 5 P1 + 2 P2 findings. User-confirmed design decisions baked into plans/261015-0431-vtx-2-jira-webhook-review-fixes: per-project start_status_ids via YAML config with dual-control error (JIRA_CONFIG_PATH vs legacy env), StatusChecker→IssueFetcher returning authoritative identity+project+status, transition target gate before dedup, typed execution_intent.created.v1 payload using changelog_id naming and occurred_at from webhook timestamp with received_at fallback, docker-compose.prod.yml overlay removing public backend port (nginx already injects gateway token + masks secret), append-only webhook_receipts table for every authenticated request (unauthenticated never writes) with 7d retention, and CI evidence running migrations/sqlc-diff/JT-01..18 at >=20 concurrent across 2 instances plus tenant Admin Webhook verification.

> Historical work record — not durable authority. Prefer docs/specs/ADRs for current decisions.
