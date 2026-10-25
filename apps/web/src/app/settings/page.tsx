"use client";

import { FormEvent, useState } from "react";
import { AppShell } from "../../components/app-shell";
import { MetricBadge } from "../../components/metric-badge";
import {
  clearOperatorCredentials,
  getApiBaseUrl,
  getOperatorCredentials,
  saveOperatorCredentials,
} from "../../lib/api";

export default function SettingsPage() {
  const [token, setToken] = useState("");
  const [operatorId, setOperatorId] = useState(() => getOperatorCredentials().operatorId);
  const [configured, setConfigured] = useState(() => Boolean(getOperatorCredentials().token));
  const [message, setMessage] = useState("");

  const save = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if (!token.trim()) {
      setMessage("Enter a token before saving.");
      return;
    }
    saveOperatorCredentials(token, operatorId);
    setToken("");
    setConfigured(true);
    setMessage("Operator credentials saved for this browser.");
  };

  const clear = () => {
    clearOperatorCredentials();
    setToken("");
    setOperatorId("");
    setConfigured(Boolean(process.env.NEXT_PUBLIC_API_AUTH_TOKEN));
    setMessage("Browser-stored credentials cleared.");
  };

  return (
    <AppShell title="Operator Settings" eyebrow="Operator Console / Settings">
      <section className="max-w-2xl rounded-lg border border-line bg-surface p-5">
        <div className="flex items-center justify-between gap-3">
          <div>
            <h2 className="text-lg font-semibold">API credentials</h2>
            <p className="mt-1 text-sm text-slate-400">Credentials are sent only in the <span className="font-mono">Authorization</span> header. They are never appended to URLs.</p>
          </div>
          <MetricBadge label={configured ? "CONFIGURED" : "NOT CONFIGURED"} variant={configured ? "success" : "warning"} />
        </div>

        <dl className="mt-4 rounded-lg border border-line bg-base p-3 text-sm">
          <dt className="text-slate-500">API base URL</dt>
          <dd className="mt-1 break-all font-mono text-xs text-slate-300">{getApiBaseUrl()}</dd>
        </dl>

        <form onSubmit={save} className="mt-5 space-y-4">
          <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
            Operator token
            <input
              type="password"
              autoComplete="off"
              className="field mt-1 w-full"
              value={token}
              onChange={(event) => setToken(event.target.value)}
              placeholder={configured ? "Enter a replacement token" : "Bearer token"}
            />
          </label>
          <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
            Operator ID
            <input
              className="field mt-1 w-full"
              maxLength={64}
              value={operatorId}
              onChange={(event) => setOperatorId(event.target.value)}
              placeholder="Display identity for audit records"
            />
          </label>
          {message ? <p role="status" className="text-sm text-slate-300">{message}</p> : null}
          <div className="flex flex-wrap gap-2">
            <button type="submit" className="button-primary focus-ring">Save credentials</button>
            <button type="button" className="button-secondary focus-ring" onClick={clear}>Clear browser credentials</button>
          </div>
        </form>

        <p className="mt-5 text-xs leading-5 text-slate-500">
          Resolution order: session storage, local storage, then <span className="font-mono">NEXT_PUBLIC_API_AUTH_TOKEN</span>. For deployments outside a private network, use a server-side session or identity provider instead of a public environment fallback.
        </p>
      </section>
    </AppShell>
  );
}
