# Jira Master Poller (MCP)

Deterministic intake for tickets in `Ready To Develope`, replacing the
LLM-per-tick Jira polling used by the Orca automation
([`jira-ready-cook-automation.md`](../jira-ready-cook-automation.md)).

Every `VTX_JIRA_POLL_INTERVAL_MS` (default **30 minutes**) the poller:

1. queries Jira through the `vtx-jira` MCP server (`mcp-atlassian`, stdio);
2. compares each issue's version (`statuscategorychangedate`, else `updated`)
   with the cursor in `var/master/jira-state.json`;
3. appends newly discovered / changed issues to `var/master/jira-queue.jsonl`
   (JSON lines, one `{discoveredAt, version, issue}` per entry);
4. advances the cursor.

No LLM is involved. The LLM/orchestration phase consumes the queue later (see
[`vtx-orchestrator-extension.md`](../design/vtx-orchestrator-extension.md) §3).

## Files and state

| Path | Purpose |
|---|---|
| `.pi/mcp.json` | MCP server config (`vtx-jira`). Gitignored — holds credentials. |
| `var/master/jira-state.json` | Seen cursor: `key -> {version, summary, status, firstSeenAt, lastSeenAt}`. |
| `var/master/jira-queue.jsonl` | Append-only handoff queue. `var/` is gitignored. |

State is written atomically (tmp + rename). The queue is appended **before**
the cursor is saved, so a crash in between re-emits entries rather than losing
them (at-least-once; downstream intake must be idempotent).

## First run is a baseline

`baselineDone=false` on the first tick: all currently matching issues are
recorded as seen and **nothing is queued**. This prevents pre-existing tickets
from being auto-dispatched. Set `VTX_MASTER_BASELINE=0` to disable, or delete
`var/master/jira-state.json` to re-baseline.

## Commands

```bash
# One-shot poll (also the health check that MCP + Jira credentials work)
make master-once

# Continuous loop in the foreground (Ctrl-C to stop)
make master

# Installed as a systemd service with automatic restart
sudo bash infra/scripts/install-jira-master.sh
journalctl -u vtx-jira-master.service -f
```

The loop ticks immediately on start, then every interval. A tick that is still
running when the next fires is skipped and logged as `jira_poll_overlap`.

## Configuration (env)

| Variable | Default | Meaning |
|---|---|---|
| `VTX_JIRA_POLL_INTERVAL_MS` | `1800000` (30 min) | Tick interval, clamped to `[1000, 24h]`. |
| `VTX_JIRA_PAGE_SIZE` | `50` | Page size, clamped to `[1, 50]` (MCP limit). |
| `VTX_JIRA_JQL` | `project = VTX AND status = "Ready To Develope" ORDER BY updated ASC` | Selection query. |
| `VTX_MASTER_STATE_DIR` | `<repo>/var/master` | Cursor + queue directory. |
| `VTX_JIRA_MCP_CONFIG` | `<repo>/.pi/mcp.json` | MCP config path. |
| `VTX_JIRA_MCP_SERVER` | `vtx-jira` | Server key inside `mcpServers`. |
| `VTX_MASTER_BASELINE` | `1` | `0` disables baseline-first-run. |

## Log lines

All output is one JSON object per line:

- `jira_master_start` — config summary (no secrets).
- `jira_poll_baseline` — first tick; `fetched`/`marked`.
- `jira_poll` — `fetched`/`queued`/`skipped`/`jql`.
- `jira_poll_overlap` — previous tick still running; skipped.
- `jira_poll_error` — tick failed (kept running, next tick retries).
- `jira_master_fatal` — start-up failure (bad config / MCP connect); exits non-zero.
- `jira_master_stop` — SIGINT/SIGTERM cleanup.

## Security

`.pi/mcp.json` contains a plaintext Jira API token and is gitignored
(tracked as `OD-ORCH-05`, pending move to a secret store + rotation). The
poller never logs the token; the systemd unit runs as the repo owner with
`NoNewPrivileges=true` and `PrivateTmp=true`.
