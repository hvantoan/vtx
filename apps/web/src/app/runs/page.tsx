"use client";

import Link from "next/link";
import { FormEvent, useEffect, useState } from "react";
import { AppShell } from "../../components/app-shell";
import { RunStateBadge } from "../../components/run-state-badge";
import { StageProgressBar } from "../../components/stage-progress-bar";
import { fetchOperatorOverview, listRuns } from "../../lib/api";
import { formatTimestamp, safeExternalUrl, shortenIdentifier } from "../../lib/dashboard";
import type { OperatorOverview, RunListItem } from "../../types/api";

const RUN_STATES = ["", "queued", "preparing", "coding", "verifying", "qa", "quality_gate", "pr_draft", "completed", "blocked", "retry_wait", "failed", "cancelled"];

export default function RunsPage() {
  const [runs, setRuns] = useState<RunListItem[]>([]);
  const [overview, setOverview] = useState<OperatorOverview | null>(null);
  const [stateFilter, setStateFilter] = useState("");
  const [issueFilter, setIssueFilter] = useState("");
  const [activeState, setActiveState] = useState("");
  const [activeIssue, setActiveIssue] = useState("");
  const [nextCursor, setNextCursor] = useState<string | null>(null);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  useEffect(() => {
    let cancelled = false;
    void Promise.all([listRuns({ limit: 50 }), fetchOperatorOverview()]).then(([runsResult, overviewResult]) => {
      if (cancelled) return;
      setRuns(runsResult.data?.items ?? []);
      setNextCursor(typeof runsResult.data?.next_cursor === "string" ? runsResult.data.next_cursor : null);
      setOverview(overviewResult.data);
      setError(runsResult.error?.message ?? overviewResult.error?.message ?? "");
      setLoading(false);
    });
    return () => { cancelled = true; };
  }, []);

  const applyFilters = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setLoading(true);
    const state = stateFilter;
    const issue = issueFilter.trim();
    const result = await listRuns({ state: state || undefined, issueKey: issue || undefined, limit: 50 });
    setRuns(result.data?.items ?? []);
    setNextCursor(typeof result.data?.next_cursor === "string" ? result.data.next_cursor : null);
    setActiveState(state);
    setActiveIssue(issue);
    setError(result.error?.message ?? "");
    setLoading(false);
  };

  const loadMore = async () => {
    if (!nextCursor) return;
    setLoading(true);
    const result = await listRuns({ state: activeState || undefined, issueKey: activeIssue || undefined, cursor: nextCursor, limit: 50 });
    setRuns((current) => [...current, ...(result.data?.items ?? [])]);
    setNextCursor(typeof result.data?.next_cursor === "string" ? result.data.next_cursor : null);
    setError(result.error?.message ?? "");
    setLoading(false);
  };

  return (
    <AppShell title="Task Runs" eyebrow="Operator Console / Runs">
      <section aria-label="Run state histogram" className="grid grid-cols-2 gap-2 sm:grid-cols-4 lg:grid-cols-6">
        {Object.entries(overview?.runs_by_state ?? {}).map(([state, count]) => (
          <div key={state} className="rounded-lg border border-line bg-surface p-3">
            <RunStateBadge state={state} />
            <p className="mt-2 font-mono text-2xl tabular-nums">{count}</p>
          </div>
        ))}
        {!overview?.runs_by_state ? <div className="col-span-full rounded-lg border border-line bg-surface p-4 font-mono text-xs text-slate-500">— histogram unavailable —</div> : null}
      </section>

      <form onSubmit={(event) => void applyFilters(event)} className="grid gap-3 rounded-lg border border-line bg-surface p-4 sm:grid-cols-[180px_1fr_auto]">
        <label className="text-xs font-semibold uppercase tracking-wider text-slate-400">
          State
          <select className="field mt-1 w-full" value={stateFilter} onChange={(event) => setStateFilter(event.target.value)}>
            {RUN_STATES.map((state) => <option key={state || "all"} value={state}>{state ? state.replaceAll("_", " ") : "all states"}</option>)}
          </select>
        </label>
        <label className="text-xs font-semibold uppercase tracking-wider text-slate-400">
          Issue key
          <input className="field mt-1 w-full" value={issueFilter} onChange={(event) => setIssueFilter(event.target.value)} placeholder="VTX-13" />
        </label>
        <button type="submit" className="button-primary focus-ring self-end" disabled={loading}>Apply filters</button>
      </form>

      {error ? <div role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-4 py-3 text-sm text-danger">{error}</div> : null}

      <section aria-label="Runs" className="space-y-2">
        {runs.map((run) => {
          const pullRequestUrl = safeExternalUrl(run.pr_url);
          return (
          <article key={run.run_id} className="rounded-lg border border-line bg-surface p-4 hover:bg-elevated">
            <div className="flex flex-wrap items-start gap-3">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2">
                  <Link href={`/runs/${encodeURIComponent(run.run_id ?? "")}`} className="focus-ring rounded font-semibold text-accent hover:text-blue-300">{run.issue_key ?? "Unknown issue"}</Link>
                  <RunStateBadge state={run.state} />
                  <span className="font-mono text-[11px] text-slate-500">attempt {run.attempt ?? "—"}</span>
                </div>
                <p className="mt-2 truncate font-mono text-xs text-slate-500">run {run.run_id ?? "—"}</p>
              </div>
              <div className="text-right font-mono text-[11px] text-slate-500">
                <p>updated {formatTimestamp(run.updated_at)}</p>
                <p>sha {shortenIdentifier(run.commit_sha)}</p>
              </div>
            </div>
            <div className="mt-3"><StageProgressBar state={run.state} /></div>
            {run.error_code ? <p className="mt-2 font-mono text-xs text-danger">{run.error_code}</p> : null}
            {pullRequestUrl ? <a href={pullRequestUrl} target="_blank" rel="noreferrer" className="mt-2 inline-block text-xs text-accent hover:underline">Open pull request</a> : null}
          </article>
          );
        })}
        {!loading && runs.length === 0 ? <div className="rounded-lg border border-line bg-surface py-12 text-center font-mono text-xs text-slate-500">— no runs matched —</div> : null}
        {loading ? <div className="rounded-lg border border-line bg-surface py-8 text-center text-sm text-slate-500">Loading measured run data…</div> : null}
      </section>

      {nextCursor ? <div className="text-center"><button type="button" className="button-secondary focus-ring" onClick={() => void loadMore()} disabled={loading}>Load more</button></div> : null}
    </AppShell>
  );
}
