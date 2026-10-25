"use client";

import { Pause, Play, X } from "lucide-react";
import { useEffect, useRef, useState } from "react";
import { openEventStream } from "../lib/api";
import { nextReconnectDelay, parseSseBlock } from "../lib/sse";
import type { BadgeVariant } from "../lib/dashboard";
import type { PlatformEvent } from "../types/api";
import { MetricBadge } from "./metric-badge";

type ConnectionState = "connected" | "reconnecting" | "disconnected";

interface SseLogViewerProps {
  runId?: string;
  onRunInvalidated?: () => void;
}

const EVENT_CLASSES: Record<string, string> = {
  "task.completed": "text-success",
  "task.failed": "text-danger",
  "task.progress": "text-accent",
  "task.created": "text-accent",
  "task.started": "text-accent",
  "task_run.created.v1": "text-accent",
  "task_run.state_changed.v1": "text-accent",
  "task_run.intent_deferred.v1": "text-warning",
  "worker.status_changed": "text-warning",
  "system.heartbeat": "text-slate-500",
};

function connectionPresentation(state: ConnectionState): { label: string; variant: BadgeVariant } {
  if (state === "connected") return { label: "CONNECTED", variant: "info" };
  if (state === "reconnecting") return { label: "RECONNECTING", variant: "warning" };
  return { label: "DISCONNECTED", variant: "destructive" };
}

function eventTime(timestamp: string): string {
  const date = new Date(timestamp);
  if (Number.isNaN(date.valueOf())) return timestamp;
  return date.toISOString().slice(11, 23);
}

export function SseLogViewer({ runId, onRunInvalidated }: SseLogViewerProps) {
  const [events, setEvents] = useState<PlatformEvent[]>([]);
  const [paused, setPaused] = useState(false);
  const [connection, setConnection] = useState<ConnectionState>("disconnected");
  const pausedRef = useRef(paused);
  const invalidateRef = useRef(onRunInvalidated);

  useEffect(() => {
    pausedRef.current = paused;
  }, [paused]);

  useEffect(() => {
    invalidateRef.current = onRunInvalidated;
  }, [onRunInvalidated]);

  useEffect(() => {
    let cancelled = false;
    let retryAttempt = 0;
    let lastEventId = "";
    let reconnectTimer: ReturnType<typeof setTimeout> | undefined;
    let activeController: AbortController | undefined;

    const scheduleReconnect = () => {
      if (cancelled) return;
      setConnection("reconnecting");
      const delay = nextReconnectDelay(retryAttempt);
      retryAttempt += 1;
      reconnectTimer = setTimeout(() => void connect(), delay);
    };

    const connect = async (): Promise<void> => {
      activeController = new AbortController();
      try {
        const response = await openEventStream(runId, lastEventId, activeController.signal);
        if (!response) {
          setConnection("disconnected");
          return;
        }
        if (!response.ok || !response.body) {
          scheduleReconnect();
          return;
        }

        retryAttempt = 0;
        setConnection("connected");
        const reader = response.body.getReader();
        const decoder = new TextDecoder();
        let buffer = "";

        while (!cancelled) {
          const chunk = await reader.read();
          if (chunk.done) break;
          buffer += decoder.decode(chunk.value, { stream: true }).replace(/\r\n/g, "\n");
          const blocks = buffer.split("\n\n");
          buffer = blocks.pop() ?? "";

          for (const block of blocks) {
            const parsed = parseSseBlock(block);
            if (parsed.id) lastEventId = parsed.id;
            if (!parsed.event || pausedRef.current) continue;
            const event = parsed.event;
            setEvents((current) => [event, ...current].slice(0, 200));
            if (
              event.type === "task_run.state_changed.v1" ||
              event.type === "pull_request.opened.v1"
            ) {
              invalidateRef.current?.();
            }
          }
        }
        scheduleReconnect();
      } catch (error: unknown) {
        if (!cancelled && !(error instanceof DOMException && error.name === "AbortError")) {
          scheduleReconnect();
        }
      }
    };

    void connect();
    return () => {
      cancelled = true;
      activeController?.abort();
      if (reconnectTimer !== undefined) clearTimeout(reconnectTimer);
    };
  }, [runId]);

  const badge = connectionPresentation(connection);

  return (
    <section aria-label="Event stream" className="flex min-h-[420px] flex-col rounded-lg border border-line bg-surface">
      <header className="flex flex-wrap items-center gap-3 border-b border-line px-4 pb-3 pt-4">
        <h2 className="text-[15px] font-semibold">Event Stream</h2>
        <MetricBadge label={badge.label} variant={badge.variant} pulse={connection === "connected"} />
        <span className="hidden font-mono text-[11px] text-slate-500 sm:inline">GET /api/v1/events</span>
        <div className="flex-1" />
        <button
          type="button"
          className="button-secondary focus-ring"
          aria-pressed={paused}
          onClick={() => setPaused((current) => !current)}
        >
          {paused ? <Play aria-hidden className="h-3.5 w-3.5" /> : <Pause aria-hidden className="h-3.5 w-3.5" />}
          {paused ? "Resume" : "Pause"}
        </button>
        <button type="button" className="button-secondary focus-ring" onClick={() => setEvents([])}>
          <X aria-hidden className="h-3.5 w-3.5" />
          Clear
        </button>
      </header>

      {connection !== "connected" ? (
        <div className="mx-4 mt-3 rounded-md border-l-2 border-warning bg-warning/10 px-3 py-2 text-[13px] text-warning">
          <span className="font-semibold">Stream {connection}.</span>{" "}
          <span className="text-slate-300">Reconnect uses exponential backoff; buffered events are retained.</span>
        </div>
      ) : null}

      <div role="log" aria-label="Platform event stream" className="log-scroll max-h-[560px] flex-1 divide-y divide-line/60 overflow-y-auto px-4 py-1">
        {events.length === 0 ? (
          <div className="py-14 text-center font-mono text-xs text-slate-500">
            — waiting for events on /api/v1/events —
          </div>
        ) : (
          events.map((event) => {
            const payload = JSON.stringify(event.data);
            return (
              <div key={`${event.id}-${event.timestamp}`} className={`event-enter py-2 ${event.type === "system.heartbeat" ? "opacity-60" : ""}`}>
                <div className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5">
                  <span className="shrink-0 font-mono text-[11px] text-slate-500">{eventTime(event.timestamp)}</span>
                  <span className={`font-mono text-xs font-medium ${EVENT_CLASSES[event.type] ?? "text-slate-400"}`}>{event.type}</span>
                  <span className="font-mono text-[11px] text-slate-500">{event.source}</span>
                  <span className="ml-auto hidden font-mono text-[11px] text-slate-500 sm:inline">id {event.id}</span>
                </div>
                <div className="mt-0.5 truncate font-mono text-[11px] text-slate-500" title={payload}>{payload}</div>
              </div>
            );
          })
        )}
      </div>
    </section>
  );
}
