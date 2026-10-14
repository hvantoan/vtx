"use client";

import { useEffect, useState, useCallback, useRef } from "react";
import {
  Activity,
  CheckCircle2,
  XCircle,
  AlertTriangle,
  RefreshCw,
  Server,
  Database,
  Workflow,
  Radio,
  Cpu,
  Terminal,
} from "lucide-react";
import type {
  HealthResponse,
  ReadyResponse,
  SystemStatusResponse,
  WorkerStatusResponse,
  PlatformEvent,
} from "../types/api";
import {
  fetchHealth,
  fetchReady,
  fetchSystemStatus,
  fetchWorkers,
} from "../lib/api";

export default function DashboardPage() {
  const [health, setHealth] = useState<HealthResponse | null>(null);
  const [healthLatency, setHealthLatency] = useState<number>(0);
  const [ready, setReady] = useState<ReadyResponse | null>(null);
  const [readyLatency, setReadyLatency] = useState<number>(0);
  const [system, setSystem] = useState<SystemStatusResponse | null>(null);
  const [workers, setWorkers] = useState<WorkerStatusResponse | null>(null);
  const [events, setEvents] = useState<PlatformEvent[]>([]);
  const [isLoading, setIsLoading] = useState<boolean>(true);
  const [isRefreshing, setIsRefreshing] = useState<boolean>(false);
  const [isPaused, setIsPaused] = useState<boolean>(false);
  const [lastUpdated, setLastUpdated] = useState<Date>(new Date());

  const pollIntervalRef = useRef<NodeJS.Timeout | null>(null);

  const loadData = useCallback(async () => {
    setIsRefreshing(true);
    const [hRes, rRes, sRes, wRes] = await Promise.all([
      fetchHealth(),
      fetchReady(),
      fetchSystemStatus(),
      fetchWorkers(),
    ]);

    setHealth(hRes.data);
    setHealthLatency(hRes.latencyMs);

    setReady(rRes.data);
    setReadyLatency(rRes.latencyMs);

    setSystem(sRes.data);
    setWorkers(wRes.data);

    setLastUpdated(new Date());
    setIsLoading(false);
    setIsRefreshing(false);
  }, []);

  useEffect(() => {
    loadData();
    pollIntervalRef.current = setInterval(loadData, 5000);
    return () => {
      if (pollIntervalRef.current) {
        clearInterval(pollIntervalRef.current);
      }
    };
  }, [loadData]);

  // Connect to SSE stream
  useEffect(() => {
    if (typeof window === "undefined") return;

    const apiBase = process.env.NEXT_PUBLIC_API_URL || "http://localhost:8080";
    let eventSource: EventSource | null = null;

    try {
      // Direct EventSource authenticated via query parameter for browser compatibility
      const token = process.env.NEXT_PUBLIC_API_AUTH_TOKEN || "vtx-dev-secret-token-change-in-production";
      eventSource = new EventSource(`${apiBase}/api/v1/events?token=${encodeURIComponent(token)}`);
      eventSource.onmessage = (event) => {
        if (isPaused) return;
        try {
          const parsed = JSON.parse(event.data) as PlatformEvent;
          setEvents((prev) => [parsed, ...prev].slice(0, 50));
        } catch {
          // ignore parse errors
        }
      };

      eventSource.addEventListener("system.heartbeat", (event: MessageEvent) => {
        if (isPaused) return;
        try {
          const parsed = JSON.parse(event.data) as PlatformEvent;
          setEvents((prev) => [parsed, ...prev].slice(0, 50));
        } catch {
          // ignore
        }
      });
    } catch {
      // Fallback: EventSource fails gracefully if unauthenticated or offline
    }

    return () => {
      eventSource?.close();
    };
  }, [isPaused]);

  const isLive = health?.status === "ok";
  const isReady = ready?.status === "ready";
  const dbStatus = ready?.dependencies?.database?.status ?? "down";
  const temporalStatus = ready?.dependencies?.temporal?.status ?? "down";

  return (
    <div className="min-h-screen bg-[#0b0f19] text-slate-100 flex flex-col font-sans">
      {/* Top Header */}
      <header className="border-b border-slate-800 bg-[#111827]/80 backdrop-blur sticky top-0 z-50 px-6 py-4 flex items-center justify-between">
        <div className="flex items-center space-x-3">
          <div className="h-9 w-9 rounded-lg bg-blue-600 flex items-center justify-center font-bold text-white shadow-lg shadow-blue-500/20">
            VTX
          </div>
          <div>
            <h1 className="text-lg font-bold tracking-tight text-white flex items-center gap-2">
              Autonomous Platform Cockpit
              <span className="text-xs px-2 py-0.5 rounded-full bg-blue-950 border border-blue-800 text-blue-400 font-mono">
                Phase 0
              </span>
            </h1>
            <p className="text-xs text-slate-400">
              Go Backend + Temporal + PostgreSQL 17 + Pi/OMP Agent Runtime
            </p>
          </div>
        </div>

        <div className="flex items-center space-x-4">
          <div className="flex items-center space-x-2 text-xs text-slate-400 font-mono">
            <Radio className={`h-3 w-3 ${isLive ? "text-emerald-400 animate-pulse" : "text-rose-500"}`} />
            <span>Poll: 5s</span>
            <span>•</span>
            <span>Last updated: {lastUpdated.toLocaleTimeString()}</span>
          </div>

          <button
            onClick={loadData}
            disabled={isRefreshing}
            className="flex items-center space-x-1.5 px-3 py-1.5 text-xs font-medium rounded-md bg-slate-800 hover:bg-slate-700 text-slate-200 border border-slate-700 transition"
          >
            <RefreshCw className={`h-3.5 w-3.5 ${isRefreshing ? "animate-spin" : ""}`} />
            <span>Refresh</span>
          </button>
        </div>
      </header>

      {/* Main Container */}
      <main className="flex-1 max-w-7xl w-full mx-auto p-6 space-y-6">
        {/* Row 1: Summary Cards */}
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
          {/* Liveness Card */}
          <div className="bg-[#111827] border border-slate-800 rounded-xl p-5 shadow-sm">
            <div className="flex items-center justify-between">
              <span className="text-xs font-semibold uppercase tracking-wider text-slate-400">
                API Liveness (/health)
              </span>
              <Server className="h-4 w-4 text-slate-500" />
            </div>
            <div className="mt-4 flex items-baseline justify-between">
              <div className="flex items-center space-x-2">
                {isLive ? (
                  <>
                    <CheckCircle2 className="h-5 w-5 text-emerald-400" />
                    <span className="text-xl font-bold text-white">LIVE</span>
                  </>
                ) : (
                  <>
                    <XCircle className="h-5 w-5 text-rose-500" />
                    <span className="text-xl font-bold text-rose-400">DOWN</span>
                  </>
                )}
              </div>
              <span className="text-xs font-mono text-slate-400">{healthLatency}ms</span>
            </div>
            <div className="mt-3 text-xs text-slate-400 flex justify-between font-mono">
              <span>Uptime:</span>
              <span>{health ? `${Math.round(health.uptime_seconds)}s` : "N/A"}</span>
            </div>
          </div>

          {/* Readiness Card */}
          <div className="bg-[#111827] border border-slate-800 rounded-xl p-5 shadow-sm">
            <div className="flex items-center justify-between">
              <span className="text-xs font-semibold uppercase tracking-wider text-slate-400">
                Orchestration (/ready)
              </span>
              <Activity className="h-4 w-4 text-slate-500" />
            </div>
            <div className="mt-4 flex items-baseline justify-between">
              <div className="flex items-center space-x-2">
                {isReady ? (
                  <>
                    <CheckCircle2 className="h-5 w-5 text-emerald-400" />
                    <span className="text-xl font-bold text-white">READY</span>
                  </>
                ) : (
                  <>
                    <AlertTriangle className="h-5 w-5 text-amber-400" />
                    <span className="text-xl font-bold text-amber-400">DEGRADED</span>
                  </>
                )}
              </div>
              <span className="text-xs font-mono text-slate-400">{readyLatency}ms</span>
            </div>
            <div className="mt-3 text-xs text-slate-400 flex justify-between font-mono">
              <span>Dependencies:</span>
              <span>
                {dbStatus === "up" && temporalStatus === "up" ? "2/2 Healthy" : "Subsystems Pending"}
              </span>
            </div>
          </div>

          {/* Temporal Workers */}
          <div className="bg-[#111827] border border-slate-800 rounded-xl p-5 shadow-sm">
            <div className="flex items-center justify-between">
              <span className="text-xs font-semibold uppercase tracking-wider text-slate-400">
                Temporal Workers
              </span>
              <Workflow className="h-4 w-4 text-slate-500" />
            </div>
            <div className="mt-4 flex items-baseline justify-between">
              <div className="flex items-center space-x-2">
                <span className="text-xl font-bold text-white">
                  {workers?.workers?.length ?? 0} Active
                </span>
              </div>
              <span className="text-xs px-2 py-0.5 rounded bg-emerald-950 border border-emerald-800 text-emerald-400 font-mono">
                Healthy
              </span>
            </div>
            <div className="mt-3 text-xs text-slate-400 flex justify-between font-mono truncate">
              <span>Queue:</span>
              <span className="text-slate-300 truncate max-w-[140px]">
                {workers?.workers?.[0]?.queue ?? "vtx-task-queue"}
              </span>
            </div>
          </div>

          {/* System Runtime */}
          <div className="bg-[#111827] border border-slate-800 rounded-xl p-5 shadow-sm">
            <div className="flex items-center justify-between">
              <span className="text-xs font-semibold uppercase tracking-wider text-slate-400">
                Go Runtime Engine
              </span>
              <Cpu className="h-4 w-4 text-slate-500" />
            </div>
            <div className="mt-4 flex items-baseline justify-between">
              <span className="text-lg font-bold font-mono text-white">
                {system?.go_version ?? "Go 1.25+"}
              </span>
              <span className="text-xs font-mono text-slate-400">
                {system?.goroutines ? `${system.goroutines} gr` : ""}
              </span>
            </div>
            <div className="mt-3 text-xs text-slate-400 flex justify-between font-mono">
              <span>Version:</span>
              <span>{system?.version ?? "1.0.0"} ({system?.environment ?? "dev"})</span>
            </div>
          </div>
        </div>

        {/* Row 2: Dependencies Deep Dive */}
        <div className="bg-[#111827] border border-slate-800 rounded-xl p-6 shadow-sm">
          <h2 className="text-sm font-semibold uppercase tracking-wider text-slate-400 mb-4 flex items-center gap-2">
            <Database className="h-4 w-4 text-blue-400" />
            Platform Subsystem & Dependency Probes
          </h2>

          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            {/* PostgreSQL Probe */}
            <div className="border border-slate-800 rounded-lg p-4 bg-[#0d1320] flex items-start justify-between">
              <div>
                <div className="flex items-center space-x-2">
                  <Database className="h-4 w-4 text-blue-400" />
                  <span className="font-semibold text-white text-sm">PostgreSQL 17 (Platform DB)</span>
                </div>
                <p className="text-xs text-slate-400 mt-1">
                  Database: <span className="font-mono text-slate-300">vtx_platform</span> (Goose Migrations + pgx)
                </p>
                {ready?.dependencies?.database?.error && (
                  <p className="text-xs text-rose-400 mt-2 font-mono">
                    Error: {ready.dependencies.database.error}
                  </p>
                )}
              </div>
              <div className="text-right">
                <span
                  className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-mono font-medium ${
                    dbStatus === "up"
                      ? "bg-emerald-950 border border-emerald-800 text-emerald-400"
                      : "bg-rose-950 border border-rose-800 text-rose-400"
                  }`}
                >
                  {dbStatus.toUpperCase()}
                </span>
                <p className="text-xs text-slate-400 mt-1 font-mono">
                  {ready?.dependencies?.database?.latency_ms ?? 0}ms
                </p>
              </div>
            </div>

            {/* Temporal Server Probe */}
            <div className="border border-slate-800 rounded-lg p-4 bg-[#0d1320] flex items-start justify-between">
              <div>
                <div className="flex items-center space-x-2">
                  <Workflow className="h-4 w-4 text-purple-400" />
                  <span className="font-semibold text-white text-sm">Temporal Orchestration Server</span>
                </div>
                <p className="text-xs text-slate-400 mt-1">
                  gRPC: <span className="font-mono text-slate-300">localhost:7233</span> (Web UI @ :8080)
                </p>
                {ready?.dependencies?.temporal?.error && (
                  <p className="text-xs text-amber-400 mt-2 font-mono">
                    Note: {ready.dependencies.temporal.error}
                  </p>
                )}
              </div>
              <div className="text-right">
                <span
                  className={`inline-flex items-center px-2 py-0.5 rounded text-xs font-mono font-medium ${
                    temporalStatus === "up"
                      ? "bg-emerald-950 border border-emerald-800 text-emerald-400"
                      : "bg-amber-950 border border-amber-800 text-amber-400"
                  }`}
                >
                  {temporalStatus.toUpperCase()}
                </span>
                <p className="text-xs text-slate-400 mt-1 font-mono">
                  {ready?.dependencies?.temporal?.latency_ms ?? 0}ms
                </p>
              </div>
            </div>
          </div>
        </div>

        {/* Row 3: Live Event Stream Feed */}
        <div className="bg-[#111827] border border-slate-800 rounded-xl p-6 shadow-sm">
          <div className="flex items-center justify-between mb-4">
            <h2 className="text-sm font-semibold uppercase tracking-wider text-slate-400 flex items-center gap-2">
              <Terminal className="h-4 w-4 text-emerald-400" />
              Live Telemetry & Server-Sent Events (SSE)
            </h2>
            <div className="flex items-center space-x-2">
              <button
                onClick={() => setIsPaused(!isPaused)}
                className="px-2.5 py-1 text-xs rounded bg-slate-800 text-slate-300 hover:bg-slate-700 transition"
              >
                {isPaused ? "Resume" : "Pause"}
              </button>
              <button
                onClick={() => setEvents([])}
                className="px-2.5 py-1 text-xs rounded bg-slate-800 text-slate-300 hover:bg-slate-700 transition"
              >
                Clear
              </button>
            </div>
          </div>

          <div className="bg-[#090d16] border border-slate-800 rounded-lg p-4 font-mono text-xs h-64 overflow-y-auto space-y-2">
            {events.length === 0 ? (
              <div className="text-slate-500 text-center py-12">
                No events streamed yet. Backend will broadcast heartbeats and agent execution events.
              </div>
            ) : (
              events.map((evt, idx) => (
                <div key={idx} className="flex items-start space-x-3 text-slate-300 border-b border-slate-900 pb-1">
                  <span className="text-slate-500 whitespace-nowrap">
                    {new Date(evt.timestamp).toLocaleTimeString()}
                  </span>
                  <span className="px-1.5 py-0.5 rounded text-[10px] bg-blue-950 text-blue-400 border border-blue-800 whitespace-nowrap">
                    {evt.type}
                  </span>
                  <span className="text-slate-400 truncate flex-1">
                    {JSON.stringify(evt.data)}
                  </span>
                </div>
              ))
            )}
          </div>
        </div>
      </main>

      {/* Footer */}
      <footer className="border-t border-slate-800/80 bg-[#0d1320] px-6 py-4 text-xs text-slate-500 flex items-center justify-between">
        <div>VTX Autonomous Development Pipeline • Monorepo Architecture v1.0.0</div>
        <div>SPEC-001 v1.2 Compliant • Phase 0 Bootstrap</div>
      </footer>
    </div>
  );
}
