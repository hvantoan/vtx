# VTX Platform — Design System & UX/AX Guidelines

**System:** VTX Platform Cockpit  
**Target:** Operations & Orchestration Telemetry Dashboard  
**Status:** Approved  

---

## 1. Brand Tokens & Color Palette

The interface is inspired by high-density developer cockpits, mission-critical infrastructure observability tools, and modern dark-mode terminal aesthetics.

| Token | Hex Value | Purpose | WCAG Contrast (vs Background) |
|---|---|---|---|
| `--background` | `#0B0F19` | Main page background (deep midnight slate) | Base |
| `--surface` | `#111827` | Primary card background | Elevated |
| `--surface-elevated` | `#1E293B` | Interactive elements, borders, pill chips | 4.8:1 |
| `--accent` | `#3B82F6` | Primary interactive actions, highlights | 6.2:1 |
| `--success` | `#10B981` | Healthy status (200 OK, Ready, Succeeded) | 7.4:1 |
| `--warning` | `#F59E0B` | Degraded, reconnecting, retrying | 8.1:1 |
| `--destructive` | `#EF4444` | Down, disconnected, failed | 5.5:1 |
| `--text-primary` | `#F8FAFC` | Primary headings and values | 14.2:1 |
| `--text-secondary` | `#94A3B8` | Subtitles, labels, timestamps | 7.1:1 |

---

## 2. Typography

- **UI & Prose:** `-apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", sans-serif`
- **Telemetry & Code:** `ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, "Liberation Mono", "Courier New", monospace`
- **Tabular Figures:** All numeric metrics (latencies, counts, uptimes) use `tabular-nums` to eliminate layout shift during live poll intervals.

---

## 3. Component Hierarchy

1. **Top Cockpit Bar:** Sticky header containing platform status, environment pill, live poll heartbeat pulse, and manual refresh trigger.
2. **Health Matrix:** 4 responsive cards showing API Liveness, Orchestration Readiness, Worker Pools, and Runtime metrics.
3. **Subsystem Probes:** Deep-dive probe cards for PostgreSQL 17 and Temporal Server with latency meters and error disclosures.
4. **Telemetry Event Stream:** Terminal-styled event log box with pause/clear stream controls and auto-scrolling log items.

---

## 4. AI-Experience (AX) & Discoverability

- `robots.txt`: Defined at `/robots.txt`
- `sitemap.xml`: Defined at `/sitemap.xml`
- `llms.txt`: Machine-readable index at `/llms.txt` documenting platform API endpoints and operations
- OpenAPI 3.1: Complete schema exposed at `contracts/openapi/v1/openapi.yaml`
