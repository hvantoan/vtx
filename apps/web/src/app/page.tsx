"use client";

import { useEffect, useState } from "react";
import { AppShell } from "../components/app-shell";
import { MetricBadge } from "../components/metric-badge";
import { OutboxPanel } from "../components/outbox-panel";
import { SseLogViewer } from "../components/sse-log-viewer";
import {
  fetchHealth,
  fetchOperatorOverview,
  fetchOperatorWorkers,
  fetchReady,
  fetchSystemStatus,
  fetchWorkers,
} from "../lib/api";
import { formatDuration, formatLatency, formatTimestamp } from "../lib/dashboard";
import type {
  DependencyCheck,
  HealthResponse,
  OperatorOverview,
  OperatorWorkerStatus,
  ReadyResponse,
  SystemStatusResponse,
  WorkerStatusResponse,
} from "../types/api";

interface Probe<T> {
  data: T | null;
  latencyMs: number;
  error: string;
  checkedAt: string;
}

const EMPTY_PROBE = { data: null, latencyMs: 0, error: "", checkedAt: "" };

function dependencyPresentation(status: string | undefined) {
  if (status === "up") return { label: "UP", variant: "success" as const };
  if (status === "degraded") return { label: "DEGRADED", variant: "warning" as const };
  if (status === "down") return { label: "DOWN", variant: "destructive" as const };
  return { label: "UNKNOWN", variant: "neutral" as const };
}

function HealthCard({ label, endpoint, badge, latency, checkedAt, meta }: {
  label: string;
  endpoint: string;
  badge: { label: string; variant: "success" | "warning" | "destructive" | "neutral" };
  latency?: number;
  checkedAt: string;
  meta: string;
}) {
  return (
    <section aria-label={label} className={`flex min-h-32 flex-col gap-3 rounded-lg border bg-surface p-4 ${badge.variant === "destructive" ? "border-danger/60" : "border-line"}`}>
      <div className="text-[11px] font-semibold uppercase tracking-wider text-slate-400">
        {label} <span className="font-mono font-normal normal-case tracking-normal text-slate-500">· {endpoint}</span>
      </div>
      <div className="flex items-center gap-2.5">
        <MetricBadge label={badge.label} variant={badge.variant} size="md" pulse={badge.variant === "success"} />
        {latency !== undefined ? <span className="ml-auto font-mono text-xs text-slate-400">{formatLatency(latency)}</span> : null}
      </div>
      <div className="mt-auto font-mono text-xs text-slate-500">{meta} · checked {formatTimestamp(checkedAt)}</div>
    </section>
  );
}

function DependencyRow({ name, endpoint, check }: { name: string; endpoint: string; check?: DependencyCheck }) {
  const badge = dependencyPresentation(check?.status);
  return (
    <li className="py-3 hover:bg-elevated">
      <div className="flex items-center gap-2.5">
        <span className="text-sm font-medium">{name}</span>
        <MetricBadge label={badge.label} variant={badge.variant} />
        <span className="ml-auto font-mono text-xs text-slate-400">{formatLatency(check?.latency_ms)}</span>
      </div>
      <div className="mt-1 font-mono text-[11px] text-slate-500">{endpoint}</div>
      {check?.error ? <div className="mt-1 break-all font-mono text-[11px] text-danger">{check.error}</div> : null}
    </li>
  );
}

export default function DashboardPage() {
  const [health, setHealth] = useState<Probe<HealthResponse>>(EMPTY_PROBE);
  const [ready, setReady] = useState<Probe<ReadyResponse>>(EMPTY_PROBE);
  const [system, setSystem] = useState<SystemStatusResponse | null>(null);
  const [workers, setWorkers] = useState<WorkerStatusResponse | null>(null);
  const [operatorWorkers, setOperatorWorkers] = useState<OperatorWorkerStatus[]>([]);
  const [overview, setOverview] = useState<OperatorOverview | null>(null);
  const [uptimeSeconds, setUptimeSeconds] = useState<number | undefined>();
  const [operatorError, setOperatorError] = useState("");

  useEffect(() => {
    let cancelled = false;
    const poll = () => void fetchHealth().then((result) => {
      if (cancelled) return;
      setHealth({ data: result.data, latencyMs: result.latencyMs, error: result.error?.message ?? "", checkedAt: new Date().toISOString() });
      if (result.data) setUptimeSeconds(result.data.uptime_seconds);
    });
    poll();
    const interval = setInterval(poll, 5_000);
    return () => { cancelled = true; clearInterval(interval); };
  }, []);

  useEffect(() => {
    let cancelled = false;
    const poll = () => void Promise.all([fetchReady(), fetchWorkers(), fetchOperatorOverview(), fetchOperatorWorkers()]).then(([readyResult, workerResult, overviewResult, operatorWorkerResult]) => {
      if (cancelled) return;
      setReady({ data: readyResult.data, latencyMs: readyResult.latencyMs, error: readyResult.error?.message ?? "", checkedAt: new Date().toISOString() });
      setWorkers(workerResult.data);
      setOverview(overviewResult.data);
      setOperatorWorkers(operatorWorkerResult.data?.items ?? []);
      setOperatorError(overviewResult.error?.message ?? operatorWorkerResult.error?.message ?? "");
    });
    poll();
    const interval = setInterval(poll, 10_000);
    return () => { cancelled = true; clearInterval(interval); };
  }, []);

  useEffect(() => {
    const interval = setInterval(() => {
      setUptimeSeconds((current) => current === undefined ? undefined : current + 1);
    }, 1_000);
    return () => clearInterval(interval);
  }, []);

  useEffect(() => {
    let cancelled = false;
    const poll = () => void fetchSystemStatus().then((result) => {
      if (!cancelled) setSystem(result.data);
    });
    poll();
    const interval = setInterval(poll, 30_000);
    return () => { cancelled = true; clearInterval(interval); };
  }, []);

  const livenessBadge = health.data?.status === "ok"
    ? { label: "UP", variant: "success" as const }
    : health.error
      ? { label: "DOWN", variant: "destructive" as const }
      : { label: "UNKNOWN", variant: "neutral" as const };
  const readinessBadge = ready.data?.status === "ready"
    ? { label: "READY", variant: "success" as const }
    : ready.data?.status === "not_ready"
      ? { label: "NOT READY", variant: "destructive" as const }
      : { label: "UNKNOWN", variant: "neutral" as const };
  const runningWorkers = workers?.workers.filter((worker) => worker.status === "running").length;
  const workerBadge = !workers
    ? { label: "UNKNOWN", variant: "neutral" as const }
    : workers.status === "degraded"
      ? { label: "DEGRADED", variant: "warning" as const }
      : runningWorkers && runningWorkers > 0
        ? { label: "RUNNING", variant: "success" as const }
        : { label: "STOPPED", variant: "destructive" as const };
  const dependencies = ready.data?.dependencies;

  return (
    <AppShell title="Platform Status">
      {ready.data?.status === "not_ready" ? (
        <div role="alert" className="rounded-lg border border-danger/40 border-l-2 bg-danger/10 px-4 py-3 text-sm text-danger">
          <strong>Readiness check failing.</strong> <span className="text-slate-300">GET <span className="font-mono">/ready</span> reports one or more unavailable dependencies.</span>
        </div>
      ) : null}
      {operatorError ? (
        <div role="alert" className="rounded-lg border border-warning/40 border-l-2 bg-warning/10 px-4 py-3 text-sm text-warning">
          Operator data unavailable: <span className="text-slate-300">{operatorError}</span>
        </div>
      ) : null}

      <section aria-label="System status overview" className="grid grid-cols-1 gap-4 sm:grid-cols-2 xl:grid-cols-4">
        <HealthCard label="Liveness" endpoint="GET /health" badge={livenessBadge} latency={health.latencyMs} checkedAt={health.checkedAt} meta={health.error || "HTTP process"} />
        <HealthCard label="Readiness" endpoint="GET /ready" badge={readinessBadge} latency={ready.latencyMs} checkedAt={ready.checkedAt} meta={ready.error || `${Object.keys(dependencies ?? {}).length} dependencies`} />
        <HealthCard label="Workers" endpoint="GET /api/v1/workers" badge={workerBadge} checkedAt={ready.checkedAt} meta={`${runningWorkers ?? "—"} running · ${workers?.workers.reduce((total, worker) => total + (worker.active_workflows ?? 0), 0) ?? "—"} active`} />
        <HealthCard label="Uptime" endpoint="/health uptime_seconds" badge={livenessBadge} checkedAt={health.checkedAt} meta={`${formatDuration(uptimeSeconds)} · ${system?.go_version ?? "runtime unknown"}`} />
      </section>

      <section aria-label="Admission and run summary" className="grid grid-cols-2 gap-4 lg:grid-cols-4">
        <div className="metric-card"><span>Active slots</span><strong>{overview?.admission?.active ?? "—"}/{overview?.admission?.max ?? 3}</strong></div>
        <div className="metric-card"><span>Deferred</span><strong>{overview?.deferred_count ?? "—"}</strong></div>
        <div className="metric-card"><span>Worker heartbeats</span><strong>{overview?.workers?.up ?? "—"} up</strong></div>
        <div className="metric-card"><span>DLQ</span><strong className={overview?.outbox?.dead_lettered ? "text-danger" : ""}>{overview?.outbox?.dead_lettered ?? "—"}</strong></div>
      </section>

      <div className="grid items-start gap-4 lg:grid-cols-3">
        <div className="space-y-4 lg:col-span-1">
          <section aria-label="Dependencies" className="rounded-lg border border-line bg-surface">
            <header className="flex items-center justify-between border-b border-line px-4 py-3">
              <h2 className="text-[15px] font-semibold">Dependencies</h2>
              <span className="font-mono text-[11px] text-slate-500">GET /ready</span>
            </header>
            <ul className="divide-y divide-line px-4">
              <DependencyRow name="PostgreSQL" endpoint="database probe" check={dependencies?.database} />
              <DependencyRow name="Temporal" endpoint="temporal gRPC probe" check={dependencies?.temporal} />
              {dependencies?.outbox ? <DependencyRow name="Outbox" endpoint="measured dispatch lag" check={dependencies.outbox} /> : null}
            </ul>
          </section>

          <section aria-label="Worker heartbeats" className="rounded-lg border border-line bg-surface">
            <header className="flex items-center justify-between border-b border-line px-4 py-3">
              <h2 className="text-[15px] font-semibold">Worker Heartbeats</h2>
              <span className="font-mono text-[11px] text-slate-500">GET /operator/workers</span>
            </header>
            <ul className="divide-y divide-line px-4">
              {operatorWorkers.map((worker) => {
                const badge = dependencyPresentation(worker.status);
                return (
                  <li key={`${worker.worker_kind}-${worker.worker_id}`} className="py-3">
                    <div className="flex items-center gap-2"><span className="min-w-0 truncate font-mono text-xs">{worker.worker_id ?? "unknown"}</span><span className="ml-auto"><MetricBadge label={badge.label} variant={badge.variant} /></span></div>
                    <p className="mt-1 font-mono text-[11px] text-slate-500">{worker.worker_kind ?? "unknown"} · beat {formatTimestamp(worker.last_beat_at)}</p>
                  </li>
                );
              })}
              {operatorWorkers.length === 0 ? <li className="py-6 text-center font-mono text-xs text-slate-500">— no heartbeat rows returned —</li> : null}
            </ul>
          </section>
        </div>
        <div className="lg:col-span-2"><SseLogViewer /></div>
      </div>

      <OutboxPanel />

      <footer className="flex flex-wrap gap-x-4 gap-y-1 border-t border-line pb-4 pt-4 font-mono text-[11px] text-slate-500">
        <span>version <span className="text-slate-400">{system?.version ?? "—"}</span></span>
        <span>commit <span className="text-slate-400">{system?.commit ?? "—"}</span></span>
        <span>env <span className="text-slate-400">{system?.environment ?? "—"}</span></span>
        <span>{system?.goroutines ?? "—"} goroutines</span>
        <span className="ml-auto">polls /health 5s · /ready 10s · /workers 10s · /status 30s</span>
      </footer>
    </AppShell>
  );
}
