"use client";

import { useState } from "react";
import { syncPullRequest } from "../lib/api";

interface PullRequestSyncProps {
  runId: string;
  onSynced: () => Promise<void> | void;
}

export function PullRequestSync({ runId, onSynced }: PullRequestSyncProps) {
  const [confirming, setConfirming] = useState(false);
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  const sync = async () => {
    setSubmitting(true);
    setError("");
    const result = await syncPullRequest(runId);
    setSubmitting(false);
    if (result.error) {
      setError(result.error.message);
      return;
    }
    setConfirming(false);
    await onSynced();
  };

  return (
    <div>
      <button type="button" className="button-secondary focus-ring" onClick={() => { setConfirming(true); setError(""); }}>Sync PR status</button>
      {confirming ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4" role="presentation">
          <section role="dialog" aria-modal="true" aria-labelledby="pr-sync-title" className="w-full max-w-md rounded-lg border border-line-strong bg-surface p-5 shadow-floating">
            <h2 id="pr-sync-title" className="text-lg font-semibold">Confirm pull request sync</h2>
            <p className="mt-2 text-sm text-slate-400">Fetch fresh pull request and CI status from GitHub. This authenticated operator action is audited and may be rate limited.</p>
            {error ? <p role="alert" className="mt-3 text-sm text-danger">{error}</p> : null}
            <div className="mt-4 flex justify-end gap-2">
              <button type="button" className="button-secondary focus-ring" disabled={submitting} onClick={() => setConfirming(false)}>Back</button>
              <button type="button" className="button-primary focus-ring" disabled={submitting} onClick={() => void sync()}>{submitting ? "Syncing…" : "Confirm sync"}</button>
            </div>
          </section>
        </div>
      ) : null}
    </div>
  );
}
