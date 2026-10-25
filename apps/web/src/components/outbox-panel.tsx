"use client";

import { useCallback, useEffect, useState } from "react";
import { fetchOutboxStats, listDlq, redriveDlq } from "../lib/api";
import type { DlqEvent, OutboxTypeCounters } from "../types/api";
import { MetricBadge } from "./metric-badge";

export function OutboxPanel() {
  const [stats, setStats] = useState<Record<string, OutboxTypeCounters>>({});
  const [dlq, setDlq] = useState<DlqEvent[]>([]);
  const [selected, setSelected] = useState<DlqEvent | null>(null);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const [submitting, setSubmitting] = useState(false);

  const load = useCallback(async () => {
    const [statsResult, dlqResult] = await Promise.all([fetchOutboxStats(), listDlq()]);
    setStats(statsResult.data ?? {});
    setDlq(dlqResult.data?.items ?? []);
    setError(statsResult.error?.message ?? dlqResult.error?.message ?? "");
    setLoading(false);
  }, []);

  useEffect(() => {
    let cancelled = false;
    void Promise.all([fetchOutboxStats(), listDlq()]).then(([statsResult, dlqResult]) => {
      if (cancelled) return;
      setStats(statsResult.data ?? {});
      setDlq(dlqResult.data?.items ?? []);
      setError(statsResult.error?.message ?? dlqResult.error?.message ?? "");
      setLoading(false);
    });
    return () => {
      cancelled = true;
    };
  }, []);

  const redrive = async () => {
    if (!selected?.outbox_id || reason.trim().length < 3) return;
    setSubmitting(true);
    setError("");
    const result = await redriveDlq(selected.outbox_id, reason.trim());
    setSubmitting(false);
    if (result.error) {
      setError(result.error.message);
      return;
    }
    setSelected(null);
    setReason("");
    setLoading(true);
    await load();
  };

  return (
    <section aria-label="Outbox operations" className="rounded-lg border border-line bg-surface">
      <header className="flex items-center justify-between border-b border-line px-4 py-3">
        <div>
          <h2 className="text-[15px] font-semibold">Outbox &amp; DLQ</h2>
          <p className="font-mono text-[11px] text-slate-500">GET /api/v1/operator/outbox</p>
        </div>
        <MetricBadge label={dlq.length > 0 ? `${dlq.length} DLQ` : "DLQ CLEAR"} variant={dlq.length > 0 ? "destructive" : "success"} />
      </header>

      {error ? <p role="alert" className="mx-4 mt-3 rounded-md border-l-2 border-danger bg-danger/10 px-3 py-2 text-sm text-danger">{error}</p> : null}
      {loading ? <div className="p-4 text-sm text-slate-500">Loading measured outbox counters…</div> : (
        <div className="overflow-x-auto">
          <table className="w-full min-w-[640px] text-left text-sm">
            <thead className="text-[11px] uppercase tracking-wider text-slate-500">
              <tr className="border-b border-line">
                <th className="px-4 py-2 font-semibold">Event type</th>
                <th className="px-3 py-2 font-semibold">Pending</th>
                <th className="px-3 py-2 font-semibold">Retrying</th>
                <th className="px-3 py-2 font-semibold">Dead lettered</th>
                <th className="px-4 py-2 font-semibold">Oldest age</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-line">
              {Object.entries(stats).map(([eventType, counters]) => (
                <tr key={eventType} className="hover:bg-elevated">
                  <td className="px-4 py-2 font-mono text-xs text-slate-300">{eventType}</td>
                  <td className="px-3 py-2 font-mono tabular-nums">{counters.pending ?? 0}</td>
                  <td className="px-3 py-2 font-mono tabular-nums">{counters.retrying ?? 0}</td>
                  <td className="px-3 py-2 font-mono tabular-nums text-danger">{counters.dead_lettered ?? 0}</td>
                  <td className="px-4 py-2 font-mono tabular-nums text-slate-400">{counters.oldest_age_seconds ?? 0}s</td>
                </tr>
              ))}
              {Object.keys(stats).length === 0 ? <tr><td colSpan={5} className="px-4 py-6 text-center font-mono text-xs text-slate-500">— no outbox counters returned —</td></tr> : null}
            </tbody>
          </table>
        </div>
      )}

      {dlq.length > 0 ? (
        <div className="border-t border-line p-4">
          <h3 className="text-xs font-semibold uppercase tracking-wider text-slate-400">Dead-lettered events</h3>
          <ul className="mt-2 divide-y divide-line">
            {dlq.map((item) => (
              <li key={item.outbox_id} className="flex flex-wrap items-center gap-3 py-3">
                <div className="min-w-0 flex-1">
                  <p className="truncate font-mono text-xs text-slate-200">{item.event_type ?? "unknown event"}</p>
                  <p className="mt-1 truncate font-mono text-[11px] text-slate-500">{item.outbox_id} · attempts {item.attempt ?? 0} · {item.dead_letter_reason ?? "no reason"}</p>
                </div>
                <button type="button" className="button-danger focus-ring" onClick={() => { setSelected(item); setReason(""); setError(""); }}>Redrive</button>
              </li>
            ))}
          </ul>
        </div>
      ) : null}

      {selected ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4" role="presentation">
          <section role="dialog" aria-modal="true" aria-labelledby="redrive-title" className="w-full max-w-md rounded-lg border border-line-strong bg-surface p-5 shadow-floating">
            <h2 id="redrive-title" className="text-lg font-semibold">Confirm DLQ redrive</h2>
            <p className="mt-2 text-sm text-slate-400">Redrive is authenticated, operator-only, and audited. It is never retried automatically by this console.</p>
            <label htmlFor="redrive-reason" className="mt-4 block text-xs font-semibold uppercase tracking-wider text-slate-400">Reason</label>
            <textarea id="redrive-reason" className="field mt-1 w-full" rows={4} maxLength={500} value={reason} onChange={(event) => setReason(event.target.value)} placeholder="At least 3 characters" />
            {error ? <p role="alert" className="mt-2 text-sm text-danger">{error}</p> : null}
            <div className="mt-4 flex justify-end gap-2">
              <button type="button" className="button-secondary focus-ring" disabled={submitting} onClick={() => setSelected(null)}>Back</button>
              <button type="button" className="button-danger focus-ring" disabled={submitting || reason.trim().length < 3} onClick={() => void redrive()}>{submitting ? "Redriving…" : "Confirm redrive"}</button>
            </div>
          </section>
        </div>
      ) : null}
    </section>
  );
}
