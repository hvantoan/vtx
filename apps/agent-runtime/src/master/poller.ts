import { appendQueue, loadState, queuePath, saveState, statePath } from "./state.js";
import type { QueueEntry, SeenIssue } from "./state.js";
import { issueVersion } from "./types.js";
import type { JiraIssue, JiraSource } from "./types.js";

export interface PollDeps {
  source: JiraSource;
  stateDir: string;
  jql: string;
  pageSize: number;
  baseline: boolean;
  now: () => Date;
  log: (event: Record<string, unknown>) => void;
}

export interface PollResult {
  fetched: number;
  queued: number;
  skipped: number;
  baseline: boolean;
}

async function fetchAll(source: JiraSource, jql: string, pageSize: number): Promise<JiraIssue[]> {
  const byKey = new Map<string, JiraIssue>();
  for (let page = 0; ; page += 1) {
    const startAt = page * pageSize;
    const result = await source.search(jql, pageSize, startAt);
    for (const issue of result.issues) byKey.set(issue.key, issue);
    if (result.issues.length < pageSize || (result.total !== undefined && startAt + result.issues.length >= result.total)) break;
  }
  return [...byKey.values()];
}

function markSeen(state: { seen: Record<string, SeenIssue> }, issue: JiraIssue, nowIso: string): void {
  const previous = state.seen[issue.key];
  state.seen[issue.key] = {
    key: issue.key,
    version: issueVersion(issue),
    summary: issue.summary,
    status: issue.status,
    firstSeenAt: previous?.firstSeenAt || nowIso,
    lastSeenAt: nowIso,
  };
}

export async function pollOnce(deps: PollDeps): Promise<PollResult> {
  const state = await loadState(statePath(deps.stateDir));
  const issues = await fetchAll(deps.source, deps.jql, deps.pageSize);
  const nowIso = deps.now().toISOString();
  const isBaseline = deps.baseline && !state.baselineDone;

  const fresh = isBaseline
    ? []
    : issues.filter((issue) => state.seen[issue.key]?.version !== issueVersion(issue));

  if (isBaseline) {
    for (const issue of issues) markSeen(state, issue, nowIso);
    state.baselineDone = true;
    state.updatedAt = nowIso;
    await saveState(statePath(deps.stateDir), state);
    deps.log({ event: "jira_poll_baseline", fetched: issues.length, marked: issues.length });
    return { fetched: issues.length, queued: 0, skipped: issues.length, baseline: true };
  }

  const entries: QueueEntry[] = fresh.map((issue) => ({
    discoveredAt: nowIso,
    version: issueVersion(issue),
    issue,
  }));
  await appendQueue(queuePath(deps.stateDir), entries);

  for (const issue of issues) markSeen(state, issue, nowIso);
  state.baselineDone = true;
  state.updatedAt = nowIso;
  await saveState(statePath(deps.stateDir), state);

  const result: PollResult = {
    fetched: issues.length,
    queued: entries.length,
    skipped: issues.length - entries.length,
    baseline: false,
  };
  deps.log({ event: "jira_poll", ...result, jql: deps.jql });
  return result;
}
