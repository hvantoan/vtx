# VTX Platform — Web Dashboard Design Guidelines

**Document Version:** 1.0.0
**Phase:** Phase 0 (Bootstrap / VTX-14)
**Status:** Approved
**Applies To:** `apps/web` (Next.js 15 + React 19 + Tailwind CSS)
**Reference Wireframe:** [`docs/wireframe/dashboard.html`](../wireframe/dashboard.html)

---

## 1. Design Principles

The dashboard is an **operator console** for the VTX Autonomous Development Pipeline. It answers one question at a glance: *"Is the platform healthy, and if not, what is failing?"*

1. **Status first, decoration never.** Every visual element encodes state. Color, shape, and text are always redundant channels — a status is never conveyed by color alone.
2. **Dark-first, terminal-adjacent.** The platform's primary users are engineers. The UI borrows its vocabulary from terminals, observability tools (Grafana, Temporal UI), and IDEs — not from marketing sites.
3. **Density over whitespace.** Monitoring UIs should fit the critical path on one viewport at 1280×800. Whitespace is used to group, not to pad.
4. **Honest latency.** Every number on screen is labeled with where it came from (`/health`, `/ready`, `/api/v1/events`) and when it was last polled. Stale data is visually distinct from live data.
5. **Calm until broken.** The healthy state is quiet: emerald accents, no glow, no motion beyond a subtle live pulse. Degraded states escalate saturation and introduce the warning/destructive palette — never both decoratively.

---

## 2. Design Tokens

### 2.1 Color System

Dark theme is the only theme in Phase 0. All values meet WCAG 2.1 AA against their intended surfaces.

#### Surface & Background

| Token | Hex | Usage |
|---|---|---|
| `bg/base` | `#0B0F19` | Page background. Near-black slate, OLED-friendly without pure black smearing. |
| `bg/surface` | `#111827` | Cards, panels, log viewer chrome. |
| `bg/elevated` | `#1A2234` | Nested surfaces, hover states, table row hover, dropdowns. |
| `border/default` | `#1E293B` | Hairline borders (1px) on cards, dividers, table rules. |
| `border/strong` | `#334155` | Input borders, emphasized dividers. |

#### Text

| Token | Hex | Contrast on `#0B0F19` | Usage |
|---|---|---|---|
| `text/primary` | `#F1F5F9` | ~14:1 | Headings, primary values, labels. |
| `text/secondary` | `#94A3B8` | ~6.6:1 | Secondary labels, descriptions, timestamps context. |
| `text/tertiary` | `#64748B` | ~4.0:1 | Tertiary meta only — never for essential information, never below 12px equivalent usage; pair with a stronger sibling for the actual value. |

#### Accent & Status

| Token | Hex | Usage |
|---|---|---|
| `accent` | `#3B82F6` | Primary actions, focus rings, links, "informational" event types, SSE connected pill. |
| `accent/hover` | `#2563EB` | Hover on accent-filled controls. |
| `accent/subtle` | `#3B82F6` @ 12% alpha | Badge/pill backgrounds for info state. |
| `success` | `#10B981` | `up`, `ready`, `ok`, `running`, `connected`, `task.completed`. |
| `success/subtle` | `#10B981` @ 12% alpha | Success badge background. |
| `warning` | `#F59E0B` | `degraded`, `idle`, `reconnecting`, `worker.status_changed` events. |
| `warning/subtle` | `#F59E0B` @ 12% alpha | Warning badge background. |
| `destructive` | `#EF4444` | `down`, `not_ready`, `stopped`, `error`, `task.failed`, HTTP 5xx. |
| `destructive/subtle` | `#EF4444` @ 12% alpha | Destructive badge background, alert banner fill. |
| `neutral` | `#64748B` | `system.heartbeat`, inactive/unknown state, placeholder text. |

#### Status Mapping (Normative)

| API value (`status` enums in `contracts/openapi/v1`) | Token | Badge label |
|---|---|---|
| `up`, `ready`, `ok`, `running`, `healthy` | `success` | UP / READY / RUNNING |
| `degraded`, `idle`, `reconnecting` | `warning` | DEGRADED / IDLE |
| `down`, `not_ready`, `stopped`, `error` | `destructive` | DOWN / NOT READY |
| unknown / unpolled | `neutral` | UNKNOWN |

Rules:
- Status color **always** appears with a text label and (for the live indicators) a shape or icon. Never a bare colored dot.
- `*/subtle` backgrounds are only for badges/pills — never full-bleed fills. The only exception is the degraded-state alert banner, which uses `destructive` at 10% alpha with a `destructive` 1px left border.
- No gradients on functional surfaces. No colored glows except a ≤6px `box-shadow` on the SSE "live" dot.

### 2.2 Typography

Two families, loaded from Google Fonts with the `vietnamese` subset included (both families ship Vietnamese diacritics):

```html
<link href="https://fonts.googleapis.com/css2?family=Plus+Jakarta+Sans:wght@400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
```

| Role | Family | Weights | Usage |
|---|---|---|---|
| `font-sans` | **Plus Jakarta Sans** | 400 / 500 / 600 / 700 / 800 | Body, headings, labels, UI chrome. |
| `font-mono` | **JetBrains Mono** | 400 / 500 / 600 | Metrics, latencies, timestamps, HTTP codes, endpoints, event types, IDs, payloads — anything a human might copy into a terminal. |

Normative rules:
- **All numeric operational values** (latency ms, uptime, goroutines, worker counts, port numbers) render in `font-mono` with `font-variant-numeric: tabular-nums` so columns don't jitter while ticking.
- **All timestamps** render in `font-mono` at `text/secondary`, ISO-8601 or `HH:MM:SS.mmm`.
- **All endpoint paths** (`/health`, `/ready`, `/api/v1/events`) render in `font-mono`.
- JetBrains Mono is never used for prose longer than a single line of a log payload; Plus Jakarta Sans is never used for metrics.

#### Type Scale

| Name | Size / Line-height | Weight | Usage |
|---|---|---|---|
| `display` | 28 / 36 | 700 | Page title ("Platform Status"). |
| `title` | 18 / 28 | 600 | Card titles, section headers. |
| `label` | 13 / 20 | 600, `tracking-wide`, uppercase | Card eyebrow labels, table column headers, badge text (11–12px for badges). |
| `body` | 14 / 22 | 400 | Descriptions, dependency names, event payloads. |
| `metric` | 24 / 32 (mono) | 500 | Primary card value (uptime, worker count). |
| `meta` | 12 / 18 (mono) | 400 | Timestamps, latencies, version/commit strings. |

### 2.3 Spacing, Radius, Border, Elevation

- **Spacing:** 4px base grid. Common values: card padding `p-4`/`p-5` (16–20px), section gaps `gap-4` (16px), row padding `py-3`.
- **Radius:** `rounded-md` (6px) on badges and pills; `rounded-lg` (8px) on cards and panels; `rounded-full` on status dots and the scenario toggle track. Nothing exceeds 8px on functional surfaces.
- **Borders:** 1px `border/default` hairlines define cards and row separators. 2px is reserved for focus rings and the alert banner's left accent.
- **Elevation:** No drop shadows on cards — depth is communicated by surface value (`base` → `surface` → `elevated`). Shadows are reserved for floating UI (dropdowns, toasts): `0 4px 16px rgba(0,0,0,0.4)`.

### 2.4 Motion

| Purpose | Spec |
|---|---|
| State transitions (badge, hover, toggle) | 150–200ms `cubic-bezier(0.4, 0, 0.2, 1)` on `color`, `background-color`, `border-color`. |
| Live status dot | 2s ease-in-out `pulse` opacity 1 → 0.4 → 1. Only on `connected`/`up` states — never on `down`. |
| New event row | 200ms fade + 4px translateY enter. No layout animation (prepend only). |
| Reconnecting spinner | 1s linear rotate on a 14px icon only. |
| `prefers-reduced-motion` | **All** animation disabled via media query; state still conveyed by label/color. |

Never animate `width`, `height`, `top`, `left`, or layout-affecting properties. Never auto-scroll content.

### 2.5 Iconography

- Lucide icons (`lucide-react` in `apps/web`), 16px default, `stroke-width: 2`.
- Status glyphs: `CheckCircle2` (up), `AlertTriangle` (degraded), `XCircle` (down), `Radio`/`Activity` (live), `RefreshCw` (reconnecting).
- No emoji anywhere in the UI. Icons are decorative (`aria-hidden`) when paired with a text label.

---

## 3. Layout & Responsive Behavior

- **Shell:** fixed top header (56px) + scrollable `main`, `max-w-7xl` (1280px) centered, `px-4 sm:px-6`.
- **Header (left→right):** wordmark, environment badge, page title — then flex-spacer — SSE connection pill, build version, mock scenario toggle.
- **Stat row:** `grid grid-cols-1 sm:grid-cols-2 xl:grid-cols-4 gap-4` — Liveness, Readiness, Workers, Uptime.
- **Content grid:** `grid lg:grid-cols-3 gap-4`. Left column (span 1): Dependencies card + Workers card stacked. Right column (span 2): SSE Log Viewer. Below `lg`, everything stacks: stat cards → dependencies → workers → log.
- **Alert banner:** full-width strip above the stat row, rendered only when readiness ≠ `ready`.
- **Breakpoints:** `sm` 640, `md` 768, `lg` 1024, `xl` 1280. Mobile-first; the dashboard must remain fully usable at 360px (cards stack, log rows wrap payload onto its own line).
- **No horizontal scrolling** at any viewport; the log viewer scrolls internally (`overflow-y-auto`), never the page horizontally.

---

## 4. Component Guidelines

### 4.1 `HealthCard`

Top-level status tile for one probe or aggregate.

**Anatomy**

```
┌─────────────────────────────────────┐
│ LABEL  (eyebrow, text/secondary)    │ ← e.g. "LIVENESS · GET /health"
│                                     │
│ ● STATUS_BADGE        200 OK  (mono)│ ← dot + MetricBadge + HTTP code
│                                     │
│ latency 1.8ms · checked 3s ago (meta)│
└─────────────────────────────────────┘
```

- **Props:** `label`, `endpoint`, `status: up|degraded|down|unknown`, `httpCode`, `latencyMs`, `lastCheckedAt`.
- **Variants:** `up` → success badge; `degraded` → warning; `down` → destructive; `unknown` → neutral.
- The status dot pulses only on `up`. On `down`, the card's left edge MAY carry a 2px `destructive` border — use sparingly, only when the card represents a hard failure.
- **States:** `loading` skeleton (pulsing `bg/elevated` blocks, no fake values); `stale` (dim the card to 60% and show `stale` meta text when `lastCheckedAt` > 3× poll interval).
- **A11y:** entire card is a `<section aria-label="Liveness">`; the badge carries `role="status"`; dot is `aria-hidden`.

### 4.2 `MetricBadge`

The atomic status indicator used inside every other component.

- **Shape:** `rounded-md`, `px-2 py-0.5`, `text-[11px] font-semibold uppercase tracking-wider`, subtle background (`{status}/subtle`), colored text + optional 6px dot.
- **Variants:** `success`, `warning`, `destructive`, `info` (accent), `neutral`.
- **Sizes:** `sm` (11px, badges in rows) and `md` (12px, card-level).
- Always includes the status **word** (`UP`, `DOWN`, `DEGRADED`, `READY`, `NOT READY`, `RUNNING`, `IDLE`, `STOPPED`) — a dot alone is a violation.
- In `apps/web` implement as a `variant` prop mapping to a token dictionary, never inline color classes at call sites.

### 4.3 `DependencyRow`

One row per entry of `ReadyResponse.dependencies` (`database`, `temporal`).

**Anatomy**

```
┌──────────────────────────────────────────────────────┐
│ ● PostgreSQL          UP        2.4ms   pgx/v5 pool  │
│   localhost:5432/vtx_platform        (endpoint, mono) │
└──────────────────────────────────────────────────────┘
```

- **Columns:** 8px status dot + name (`body`, `text/primary`) | `MetricBadge` | latency (`meta`, `tabular-nums`, right-aligned) | optional detail (`text/tertiary`, truncated).
- **Sub-line:** endpoint/DSN *display* string in mono — never real credentials (Boundary Rule 3: no secrets in the frontend; display names like `postgres:5432/vtx_platform` only).
- **Error state:** when `DependencyCheck.error` is non-null, the row expands with a second line: `destructive` text, `error` message in mono, e.g. `connection refused`.
- **Latency formatting:** `< 1ms` → `0.x ms` one decimal; `1–999` → one decimal; `≥ 1000` → `x.xx s`. `null` → `—`.
- Rows separated by `divide-y divide-line`; row `py-3`, hover `bg/elevated`.

### 4.4 `SSELogViewer`

The real-time event feed for `GET /api/v1/events` (events conform to `contracts/events/v1` `PlatformEvent`).

**Anatomy**

```
┌──────────────────────────────────────────────────────────┐
│ EVENT STREAM   ● CONNECTED        ⏸ Pause   ✕ Clear      │
├──────────────────────────────────────────────────────────┤
│ 14:32:07.412  task.completed   api        {"task_id":…}  │
│ 14:32:05.201  system.heartbeat worker     {"seq":128}    │
└──────────────────────────────────────────────────────────┘
```

- **Header:** title + connection `MetricBadge` (`CONNECTED` accent / `RECONNECTING` warning / `DISCONNECTED` destructive) + controls (pause/resume, clear). Controls are real `<button>`s ≥32px hit area with visible focus rings.
- **Event row:** grid — `meta` mono timestamp (`HH:MM:SS.mmm`) | event-type chip (`meta` mono, colored per §4.4.1) | source (`text/secondary`) | payload preview (`meta`, `text/tertiary`, mono, truncated to one line, full JSON on `title` tooltip).
- **Event-type color mapping:** `task.completed` → success; `task.failed` → destructive; `task.progress`/`task.created`/`task.started` → info; `worker.status_changed` → warning; `system.heartbeat` → neutral. `system.heartbeat` rows render at 60% opacity to keep the feed quiet.
- **Order & scroll:** newest events **prepended** at the top — the user is never force-scrolled. Max 200 rows buffered; oldest are evicted.
- **A11y:** scroll region is `role="log"` (implies `aria-live="polite"`) with `aria-label="Platform event stream"`. Pause must actually stop DOM updates (screen-reader flood control).
- **Empty state:** centered mono message `— waiting for events on /api/v1/events —` in `text/tertiary`.
- **Disconnected state:** a non-blocking inline notice row pinned at the top of the list (`warning` left border) — the buffer is retained, never cleared on reconnect.

---

## 5. Accessibility Checklist (Normative)

- [ ] Text contrast ≥ 4.5:1 (all token pairs above are verified; `text/tertiary` is meta-only).
- [ ] Status never conveyed by color alone — every indicator pairs a label.
- [ ] Focus-visible ring: `2px accent` + `2px offset bg/base` on every interactive element; never `outline: none`.
- [ ] All controls reachable and operable by keyboard; the scenario toggle is a `radiogroup` (`role="radio"`, `aria-checked`).
- [ ] `role="log"` on the event feed; `role="status"` on card badges; `aria-hidden` on decorative dots/icons.
- [ ] `prefers-reduced-motion` disables all pulses, spins, and transitions.
- [ ] Touch targets ≥ 44×44px on mobile (buttons may render visually smaller but keep the hit area).
- [ ] `lang="en"`, `<meta viewport>` with zoom enabled (no `user-scalable=no`).
- [ ] `color-scheme: dark` declared so browser chrome (scrollbars, form controls) matches.

---

## 6. Data Source Mapping

| UI element | Source | Field(s) |
|---|---|---|
| Liveness card | `GET /health` | `status`, `uptime_seconds`, `timestamp` |
| Readiness card | `GET /ready` | `status` (`ready`/`not_ready`), HTTP 200/503 |
| Dependency rows | `GET /ready` | `dependencies.database`, `dependencies.temporal` (`status`, `latency_ms`, `error`) |
| Worker count + list | `GET /api/v1/workers` | `status`, `workers[]` (`id`, `queue`, `status`, `active_workflows`) |
| Build meta | `GET /api/v1/system/status` | `version`, `commit`, `environment`, `go_version`, `goroutines` |
| Uptime counter | `GET /health` (seed) | `uptime_seconds` — ticked client-side between polls |
| Event stream | `GET /api/v1/events` (SSE, Bearer) | `PlatformEvent` (`id`, `type`, `timestamp`, `source`, `data`) |

Poll cadence (Phase 0): `/health` every 5s, `/ready` every 10s, `/api/v1/workers` every 10s, `/api/v1/system/status` every 30s. SSE is persistent with exponential-backoff reconnect (1s → 30s cap).

---

## 7. Anti-Patterns

- **Don't** introduce light theme, glassmorphism, gradients, or neon glow on functional surfaces.
- **Don't** render raw hex in components — always through the token dictionary (`MetricBadge` variants, Tailwind `theme.extend` entries).
- **Don't** auto-scroll the log, play sounds, or flash on state change — escalation is through the warning/destructive palette, not motion.
- **Don't** display real connection strings, tokens, or internal endpoints (Boundary Rule 3).
- **Don't** fabricate values during loading — skeletons, not fake zeros. A spinner next to a real number is a lie about freshness.
- **Don't** use `<div>` click handlers; every control is a semantic `<button>`.

---

## 8. Tailwind Configuration Reference

```ts
// apps/web/tailwind.config.ts — token mapping
theme: {
  extend: {
    colors: {
      base:      '#0B0F19',
      surface:   '#111827',
      elevated:  '#1A2234',
      line:      '#1E293B',
      'line-strong': '#334155',
      accent:    { DEFAULT: '#3B82F6', hover: '#2563EB' },
      success:   '#10B981',
      warning:   '#F59E0B',
      danger:    '#EF4444',
    },
    fontFamily: {
      sans: ['"Plus Jakarta Sans"', 'system-ui', 'sans-serif'],
      mono: ['"JetBrains Mono"', 'ui-monospace', 'monospace'],
    },
  },
}
```
