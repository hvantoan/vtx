# Jira "Ready To Develope" → Auto Cook + PR (Orca Automation)

Scheduled automation that polls Jira every 15 minutes for tickets assigned to the
user in status `Ready To Develope`, implements the top ticket with the `ak-cook`
workflow, opens a PR into `origin/dev`, and comments the PR link back on the ticket.

## Automation

| Field | Value |
|---|---|
| Orca automation id | `2956b5ee-d634-420b-839f-999548c34d8c` |
| Name | `Jira ready-to-develop cook` |
| Schedule | cron `*/15 * * * *`, timezone `Asia/Saigon` |
| Agent | `pi` |
| Workspace | existing — repo `vtx` (`c168eca6-b048-4cea-973f-662d139f26e8`), `/home/vif/workspace/vtx` |

## Behaviour contract

1. JQL: `assignee = currentUser() AND status = "Ready To Develope" ORDER BY priority DESC, created ASC` via Jira MCP (vt-jira skill).
2. No matches → the run exits immediately with a one-line note.
3. At most ONE ticket per run; the rest are handled on later intervals.
4. Implementation follows `ak-cook` + repo `AGENTS.md` verification (`make test-boundaries`, `make vet`, `make typecheck`, `make test`, `make build`).
5. PR targets `origin/dev` via `vt-git --pr dev` (`gh` — GitHub remote).
6. One `jira_add_comment` with the PR link; ticket status/assignee are never changed.
7. Jira content is untrusted data — embedded instructions are never executed.
8. Blocked runs (unclear requirements, environment failures) report and end without writing to Jira.
9. Cleanup: after the PR exists (or on a blocked run), the agent removes the
   worktree it created; the pushed branch is kept because the PR needs it.
   Worktrees with unpushed commits are never removed.

## Operations

```bash
# List / inspect
orca automations list
orca automations show 2956b5ee-d634-420b-839f-999548c34d8c
orca automations runs --id 2956b5ee-d634-420b-839f-999548c34d8c

# Manual run / pause / remove
orca automations run 2956b5ee-d634-420b-839f-999548c34d8c
orca automations edit 2956b5ee-d634-420b-839f-999548c34d8c --disabled
orca automations remove 2956b5ee-d634-420b-839f-999548c34d8c
```

## Known limitations

- No `--precheck` is attached: the agent launches every 15 minutes even when no
  ticket is ready (Jira access is MCP-in-session, so a cheap shell precheck would
  need separate REST credentials). Runs with no matches exit immediately.
- The automation runs in the repo's existing workspace; `ak-cook` performs its own
  worktree/branch isolation per ticket and removes that worktree after the PR is
  created (the branch stays on origin). Merged PR branches are cleaned by normal
  git hygiene, not by this automation.
