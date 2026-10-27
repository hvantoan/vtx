"use client";

import { useParams } from "next/navigation";
import { useCallback, useEffect, useState } from "react";
import { AppShell } from "../../../components/app-shell";
import { MetricBadge } from "../../../components/metric-badge";
import { OperatorActions } from "../../../components/operator-actions";
import { OutboxPanel } from "../../../components/outbox-panel";
import { PullRequestSync } from "../../../components/pull-request-sync";
import { RunStateBadge } from "../../../components/run-state-badge";
import { SseLogViewer } from "../../../components/sse-log-viewer";
import { StageProgressBar } from "../../../components/stage-progress-bar";
import { getRegistryRun, getRunDetail, listRunEvents } from "../../../lib/api";
import { formatTimestamp, getQaStatus, safeExternalUrl, shortenIdentifier } from "../../../lib/dashboard";
import type { ApiResult, CursorPage, RegistryRun, RegistryRunEvent, RunDetailView } from "../../../types/api";

interface RunPageResults {
  detail: ApiResult<RunDetailView>;
  run: ApiResult<RegistryRun>;
  events: ApiResult<CursorPage<RegistryRunEvent>>;
}

function fetchRunPage(runId: string): Promise<RunPageResults> {
  return Promise.all([getRunDetail(runId), getRegistryRun(runId), listRunEvents(runId)]).then(([detail, run, events]) => ({ detail, run, events }));
}

function metadataValue(metadata: Record<string, unknown> | undefined, key: string): string {
  const value = metadata?.[key];
  return typeof value === "string" ? value : "";
}

function evidenceVariant(value: string | null | undefined) {
  const normalized = value?.toLowerCase();
  if (["pass", "passed", "success", "successful", "green", "completed", "open"].includes(normalized ?? "")) return "success" as const;
  if (["fail", "failed", "failure", "error", "red"].includes(normalized ?? "")) return "destructive" as const;
  if (["pending", "queued", "running", "in_progress"].includes(normalized ?? "")) return "warning" as const;
  return "neutral" as const;
}

export default function RunDetailPage() {
  const params = useParams<{ runId: string }>();
  const runId = params.runId;
  const [detail, setDetail] = useState<RunDetailView | null>(null);
  const [run, setRun] = useState<RegistryRun | null>(null);
  const [events, setEvents] = useState<RegistryRunEvent[]>([]);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);

  const applyResults = useCallback((results: RunPageResults) => {
    setDetail(results.detail.data);
    setRun(results.run.data);
    setEvents([...(results.events.data?.items ?? [])].sort((left, right) => left.version - right.version));
    setError(results.detail.error?.message ?? results.run.error?.message ?? results.events.error?.message ?? "");
    setLoading(false);
  }, []);

  const reload = useCallback(async () => {
    const results = await fetchRunPage(runId);
    applyResults(results);
  }, [applyResults, runId]);

  useEffect(() => {
    let cancelled = false;
    void fetchRunPage(runId).then((results) => {
      if (!cancelled) applyResults(results);
    });
    return () => { cancelled = true; };
  }, [applyResults, runId]);

  const qaStatus = getQaStatus(run?.state ?? detail?.run?.state, detail?.gate_decision?.decision);
  const pullRequest = detail?.pull_request;
  const pullRequestUrl = safeExternalUrl(pullRequest?.pr_url);

  return (
    <AppShell title={detail?.run?.issue_key ?? "Run Detail"} eyebrow="Operator Console / Runs">
      {error ? <div role="alert" className="rounded-lg border border-danger/40 bg-danger/10 px-4 py-3 text-sm text-danger">{error}</div> : null}
      {loading ? <div className="rounded-lg border border-line bg-surface py-12 text-center text-sm text-slate-500">Loading authoritative run detail…</div> : null}

      {run ? (
        <>
          <section className="rounded-lg border border-line bg-surface p-4" aria-label="Run summary">
            <div className="flex flex-wrap items-start gap-3">
              <div className="min-w-0 flex-1">
                <div className="flex flex-wrap items-center gap-2"><RunStateBadge state={run.state} /><span className="font-mono text-xs text-slate-500">version {run.version} · attempt {run.attempt}</span></div>
                <p className="mt-2 break-all font-mono text-xs text-slate-400">{run.run_id}</p>
              </div>
              <OperatorActions run={run} onApplied={reload} />
            </div>
            <div className="mt-4"><StageProgressBar state={run.state} resumeState={run.resume_state} /></div>
          </section>

          <div className="grid items-start gap-4 lg:grid-cols-3">
            <section className="rounded-lg border border-line bg-surface p-4 lg:col-span-2" aria-label="Evidence and quality status">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <h2 className="text-[15px] font-semibold">Evidence, QA &amp; Gate</h2>
                <MetricBadge label={qaStatus.label} variant={qaStatus.variant} />
              </div>
              <div className="mt-4 grid gap-3 sm:grid-cols-2">
                <div className="nested-card"><span>Commit SHA</span><code className="select-all">{run.commit_sha || detail?.run?.commit_sha || "UNKNOWN"}</code></div>
                <div className="nested-card"><span>Branch</span><code className="select-all">{run.branch || detail?.worktree?.branch || "UNKNOWN"}</code></div>
                <div className="nested-card"><span>Worktree</span><code className="select-all">{detail?.worktree?.status ?? "UNKNOWN"}</code><small>{detail?.worktree?.path ?? "No worktree view returned"}</small></div>
                <div className="nested-card"><span>Base SHA</span><code className="select-all">{detail?.worktree?.base_sha ?? "UNKNOWN"}</code></div>
              </div>

              <div className="mt-4 rounded-lg border border-line bg-base p-4">
                <div className="flex flex-wrap items-center gap-2">
                  <h3 className="text-sm font-semibold">Quality gate</h3>
                  <MetricBadge label={detail?.gate_decision?.decision?.toUpperCase() ?? "UNKNOWN"} variant={evidenceVariant(detail?.gate_decision?.decision)} />
                </div>
                <dl className="mt-3 grid gap-2 text-sm sm:grid-cols-2">
                  <div><dt className="text-slate-500">Subject commit</dt><dd className="font-mono text-xs text-slate-300">{detail?.gate_decision?.subject_commit_sha ?? "—"}</dd></div>
                  <div><dt className="text-slate-500">Decided at</dt><dd className="font-mono text-xs text-slate-300">{formatTimestamp(detail?.gate_decision?.decided_at)}</dd></div>
                </dl>
                <details className="mt-3"><summary className="cursor-pointer text-xs text-accent">Gate evidence payload</summary><pre className="mt-2 overflow-x-auto whitespace-pre-wrap break-all rounded-md bg-elevated p-3 font-mono text-[11px] text-slate-400">{JSON.stringify(detail?.gate_decision ?? null, null, 2)}</pre></details>
                <p className="mt-3 text-xs text-slate-500">QA status is derived only from the registry state and returned gate decision; absent evidence remains UNKNOWN.</p>
              </div>
            </section>

            <div className="space-y-4">
              <section className="rounded-lg border border-line bg-surface p-4" aria-label="Pull request status">
                <div className="flex items-center justify-between gap-2"><h2 className="text-[15px] font-semibold">Pull Request</h2><MetricBadge label={pullRequest?.ci_status?.toUpperCase() ?? "CI UNKNOWN"} variant={evidenceVariant(pullRequest?.ci_status)} /></div>
                {pullRequest ? (
                  <div className="mt-3 space-y-2 text-sm">
                    <p className="font-mono text-xs text-slate-400">{pullRequest.repo_owner}/{pullRequest.repo_name} #{pullRequest.pr_number ?? "—"}</p>
                    <p><MetricBadge label={pullRequest.status?.toUpperCase() ?? "UNKNOWN"} variant={evidenceVariant(pullRequest.status)} /></p>
                    <p className="font-mono text-[11px] text-slate-500">head {shortenIdentifier(pullRequest.head_sha)} · synced {formatTimestamp(pullRequest.last_synced_at ?? undefined)}</p>
                    {pullRequestUrl ? <a href={pullRequestUrl} target="_blank" rel="noreferrer" className="inline-block text-accent hover:underline">Open pull request</a> : null}
                    <PullRequestSync runId={runId} onSynced={reload} />
                  </div>
                ) : <p className="mt-3 text-sm text-slate-500">No pull request record returned.</p>}
              </section>

              <section className="rounded-lg border border-line bg-surface p-4" aria-label="Error disposition">
                <h2 className="text-[15px] font-semibold">Error Disposition</h2>
                <dl className="mt-3 space-y-2 text-sm">
                  <div><dt className="text-slate-500">Code</dt><dd className="break-all font-mono text-xs text-danger">{detail?.last_error?.error_code || run.error_code || "—"}</dd></div>
                  <div><dt className="text-slate-500">Message</dt><dd className="break-words font-mono text-xs text-slate-300">{detail?.last_error?.error_message || run.error_message || "—"}</dd></div>
                  <div><dt className="text-slate-500">Retry due</dt><dd className="font-mono text-xs text-slate-300">{formatTimestamp(run.retry_due_at ?? undefined)}</dd></div>
                </dl>
              </section>
            </div>
          </div>

          <section aria-label="Run timeline" className="rounded-lg border border-line bg-surface">
            <header className="border-b border-line px-4 py-3"><h2 className="text-[15px] font-semibold">Audit Timeline</h2><p className="font-mono text-[11px] text-slate-500">GET /api/v1/registry/runs/{runId}/events</p></header>
            <ol role="log" aria-label="Run audit event timeline" className="divide-y divide-line px-4">
              {events.map((event) => {
                const operatorId = metadataValue(event.metadata, "operator_id");
                const reason = metadataValue(event.metadata, "reason");
                return (
                  <li key={event.version} className="grid gap-2 py-3 sm:grid-cols-[90px_1fr_auto]">
                    <span className="font-mono text-xs text-slate-500">v{event.version}</span>
                    <div>
                      <p className="text-sm"><strong>{event.action}</strong> · {event.previous_state || "created"} → {event.current_state}</p>
                      <p className="mt-1 text-xs text-slate-500">{event.actor} ({event.actor_type}){operatorId ? ` · operator ${operatorId}` : ""}</p>
                      {reason ? <p className="mt-1 text-xs text-slate-300">{reason}</p> : null}
                      {event.metadata ? <details className="mt-1"><summary className="cursor-pointer text-[11px] text-accent">metadata</summary><pre className="mt-1 whitespace-pre-wrap break-all font-mono text-[11px] text-slate-500">{JSON.stringify(event.metadata, null, 2)}</pre></details> : null}
                    </div>
                    <time className="font-mono text-[11px] text-slate-500">{formatTimestamp(event.created_at)}</time>
                  </li>
                );
              })}
              {events.length === 0 ? <li className="py-8 text-center font-mono text-xs text-slate-500">— no audit events returned —</li> : null}
            </ol>
          </section>

          <SseLogViewer runId={runId} onRunInvalidated={reload} />
          <OutboxPanel />
        </>
      ) : null}
    </AppShell>
  );
}
