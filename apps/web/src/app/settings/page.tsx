"use client";

import { useState, type FormEvent } from "react";
import { AppShell } from "../../components/app-shell";
import { MetricBadge } from "../../components/metric-badge";
import { useSession } from "../../components/session";
import { changePassword } from "../../lib/api";
import { formatTimestamp } from "../../lib/dashboard";

const MIN_PASSWORD_LENGTH = 12;

function SettingsContent() {
  const { user } = useSession();
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmPassword, setConfirmPassword] = useState("");
  const [error, setError] = useState("");
  const [message, setMessage] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError("");
    setMessage("");
    if (newPassword.length < MIN_PASSWORD_LENGTH) {
      setError(`The new password must be at least ${MIN_PASSWORD_LENGTH} characters.`);
      return;
    }
    if (newPassword !== confirmPassword) {
      setError("The new passwords do not match.");
      return;
    }

    setBusy(true);
    const result = await changePassword(currentPassword, newPassword);
    setBusy(false);
    if (result.error) {
      setError(
        result.error.error === "invalid_credentials"
          ? "The current password is incorrect."
          : result.error.message,
      );
      return;
    }
    setCurrentPassword("");
    setNewPassword("");
    setConfirmPassword("");
    setMessage("Password changed. Other sessions were signed out.");
  };

  return (
    <div className="max-w-2xl space-y-4">
      <section className="rounded-lg border border-line bg-surface p-5">
        <div className="flex items-center justify-between gap-3">
          <div>
            <h2 className="text-lg font-semibold">Account</h2>
            <p className="mt-1 text-sm text-slate-400">
              The console authenticates with an HttpOnly session cookie set by the API.
            </p>
          </div>
          <MetricBadge label="SIGNED IN" variant="success" />
        </div>

        <dl className="mt-4 divide-y divide-line rounded-lg border border-line bg-base text-sm">
          <div className="flex items-baseline justify-between gap-4 px-3 py-2.5">
            <dt className="text-slate-500">Username</dt>
            <dd className="font-mono text-xs text-slate-300">{user.username}</dd>
          </div>
          <div className="flex items-baseline justify-between gap-4 px-3 py-2.5">
            <dt className="text-slate-500">Session expires</dt>
            <dd className="font-mono text-xs text-slate-300">{formatTimestamp(user.expires_at)}</dd>
          </div>
          <div className="flex items-baseline justify-between gap-4 px-3 py-2.5">
            <dt className="text-slate-500">API access</dt>
            <dd className="font-mono text-xs text-slate-300">same-origin /api proxy</dd>
          </div>
        </dl>
      </section>

      <section className="rounded-lg border border-line bg-surface p-5">
        <h2 className="text-lg font-semibold">Change password</h2>
        <p className="mt-1 text-sm text-slate-400">
          Changing the password signs out every other browser session. This session stays active.
        </p>

        <form onSubmit={submit} className="mt-4 space-y-4">
          <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
            Current password
            <input
              className="field mt-1 w-full"
              type="password"
              autoComplete="current-password"
              value={currentPassword}
              onChange={(event) => setCurrentPassword(event.target.value)}
              required
            />
          </label>
          <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
            New password
            <input
              className="field mt-1 w-full"
              type="password"
              autoComplete="new-password"
              minLength={MIN_PASSWORD_LENGTH}
              value={newPassword}
              onChange={(event) => setNewPassword(event.target.value)}
              required
            />
          </label>
          <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
            Confirm new password
            <input
              className="field mt-1 w-full"
              type="password"
              autoComplete="new-password"
              minLength={MIN_PASSWORD_LENGTH}
              value={confirmPassword}
              onChange={(event) => setConfirmPassword(event.target.value)}
              required
            />
          </label>

          {error ? (
            <p role="alert" className="rounded-md border-l-2 border-danger bg-danger/10 px-3 py-2 text-sm text-danger">
              {error}
            </p>
          ) : null}
          {message ? <p role="status" className="text-sm text-success">{message}</p> : null}

          <button type="submit" className="button-primary focus-ring" disabled={busy}>
            {busy ? "Saving…" : "Change password"}
          </button>
        </form>

        <p className="mt-5 text-xs leading-5 text-slate-500">
          Minimum {MIN_PASSWORD_LENGTH} characters. If the password is lost, reset it on the host with{" "}
          <span className="font-mono">make user-password</span>.
        </p>
      </section>
    </div>
  );
}

export default function SettingsPage() {
  return (
    <AppShell title="Operator Settings" eyebrow="Operator Console / Settings">
      <SettingsContent />
    </AppShell>
  );
}
