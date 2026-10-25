"use client";

import { useState } from "react";
import { postTransition } from "../lib/api";
import type { OperatorAction, RegistryRun } from "../types/api";

interface OperatorActionsProps {
  run: RegistryRun;
  onApplied: () => Promise<void> | void;
}

const ACTION_LABELS: Record<OperatorAction, string> = {
  block: "Block run",
  resume: "Resume run",
  cancel: "Cancel run",
};

function availableActions(run: RegistryRun): OperatorAction[] {
  if (["completed", "failed", "cancelled"].includes(run.state)) return [];
  if (run.state === "blocked") return ["resume", "cancel"];
  return ["block", "cancel"];
}

export function OperatorActions({ run, onApplied }: OperatorActionsProps) {
  const [action, setAction] = useState<OperatorAction | null>(null);
  const [reason, setReason] = useState("");
  const [error, setError] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const actions = availableActions(run);

  const submit = async () => {
    if (!action || reason.trim().length < 3) return;
    setSubmitting(true);
    setError("");
    const result = await postTransition(run.run_id, run.version, action, reason.trim());
    setSubmitting(false);

    if (result.error) {
      setError(
        result.status === 409 && result.error.error === "stale_version"
          ? "Run changed while this dialog was open. Refreshed data; review and retry."
          : result.error.message,
      );
      if (result.status === 409) await onApplied();
      return;
    }

    setAction(null);
    setReason("");
    await onApplied();
  };

  if (actions.length === 0) {
    return <p className="text-sm text-slate-500">No operator transitions are available for this terminal run.</p>;
  }

  return (
    <div>
      <div className="flex flex-wrap gap-2">
        {actions.map((candidate) => (
          <button
            key={candidate}
            type="button"
            className={candidate === "cancel" ? "button-danger focus-ring" : "button-secondary focus-ring"}
            onClick={() => {
              setAction(candidate);
              setReason("");
              setError("");
            }}
          >
            {ACTION_LABELS[candidate]}
          </button>
        ))}
      </div>

      {action ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4" role="presentation">
          <section role="dialog" aria-modal="true" aria-labelledby="operator-action-title" className="w-full max-w-md rounded-lg border border-line-strong bg-surface p-5 shadow-floating">
            <h2 id="operator-action-title" className="text-lg font-semibold">Confirm {ACTION_LABELS[action].toLowerCase()}</h2>
            <p className="mt-2 text-sm text-slate-400">
              This authenticated action is audited against run <span className="font-mono text-slate-300">{run.run_id}</span> at version <span className="font-mono text-slate-300">{run.version}</span>.
            </p>
            <label className="mt-4 block text-xs font-semibold uppercase tracking-wider text-slate-400" htmlFor="operator-reason">Reason</label>
            <textarea
              id="operator-reason"
              value={reason}
              onChange={(event) => setReason(event.target.value)}
              maxLength={500}
              rows={4}
              className="field mt-1 w-full"
              placeholder="At least 3 characters"
            />
            {error ? <p role="alert" className="mt-2 text-sm text-danger">{error}</p> : null}
            <div className="mt-4 flex justify-end gap-2">
              <button type="button" className="button-secondary focus-ring" onClick={() => setAction(null)} disabled={submitting}>Back</button>
              <button type="button" className={action === "cancel" ? "button-danger focus-ring" : "button-primary focus-ring"} onClick={() => void submit()} disabled={submitting || reason.trim().length < 3}>
                {submitting ? "Applying…" : "Confirm action"}
              </button>
            </div>
          </section>
        </div>
      ) : null}
    </div>
  );
}
