"use client";

import { useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState, type FormEvent } from "react";
import { fetchCurrentUser, login } from "../../lib/api";

function safeNext(value: string | null): string {
  if (!value || !value.startsWith("/") || value.startsWith("//")) return "/";
  return value;
}

function LoginForm() {
  const router = useRouter();
  const searchParams = useSearchParams();
  const next = safeNext(searchParams.get("next"));
  const [username, setUsername] = useState("operator");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    let cancelled = false;
    void fetchCurrentUser().then((result) => {
      if (!cancelled && result.data) router.replace(next);
    });
    return () => {
      cancelled = true;
    };
  }, [next, router]);

  const submit = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    setError("");
    setBusy(true);
    const result = await login(username, password);
    if (result.error) {
      setBusy(false);
      setError(
        result.status === 429
          ? "Too many failed attempts. Try again in 15 minutes."
          : result.error.error === "invalid_credentials"
            ? "Invalid username or password."
            : result.error.message,
      );
      return;
    }
    router.replace(next);
    router.refresh();
  };

  return (
    <form onSubmit={submit} className="rounded-lg border border-line bg-surface p-6 shadow-xl shadow-black/20">
      <h1 className="text-lg font-semibold text-slate-100">Sign in</h1>
      <p className="mt-1 text-sm text-slate-400">Use the operator account seeded on this host.</p>

      <div className="mt-5 space-y-4">
        <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
          Username
          <input
            className="field mt-1 w-full"
            name="username"
            autoComplete="username"
            autoCapitalize="none"
            spellCheck={false}
            value={username}
            onChange={(event) => setUsername(event.target.value)}
            required
          />
        </label>
        <label className="block text-xs font-semibold uppercase tracking-wider text-slate-400">
          Password
          <input
            className="field mt-1 w-full"
            type="password"
            name="password"
            autoComplete="current-password"
            value={password}
            onChange={(event) => setPassword(event.target.value)}
            required
          />
        </label>
      </div>

      {error ? (
        <p role="alert" className="mt-4 rounded-md border-l-2 border-danger bg-danger/10 px-3 py-2 text-sm text-danger">
          {error}
        </p>
      ) : null}

      <button type="submit" className="button-primary focus-ring mt-5 w-full" disabled={busy || !username || !password}>
        {busy ? "Signing in…" : "Sign in"}
      </button>
    </form>
  );
}

export default function LoginPage() {
  return (
    <main className="flex min-h-screen items-center justify-center bg-base px-4 py-10 text-slate-100">
      <div className="w-full max-w-sm">
        <div className="mb-5 flex items-center gap-2.5">
          <span className="flex h-8 w-8 items-center justify-center rounded-md bg-accent text-sm font-extrabold text-white">
            V
          </span>
          <span className="text-[15px] font-bold tracking-tight">VTX Platform</span>
        </div>
        <Suspense
          fallback={
            <div className="rounded-lg border border-line bg-surface p-6 text-sm text-slate-400">
              Loading sign-in…
            </div>
          }
        >
          <LoginForm />
        </Suspense>
        <p className="mt-4 text-center text-[11px] text-slate-500">
          Forgot the password? Reset it on the host with <span className="font-mono">make user-password</span>.
        </p>
      </div>
    </main>
  );
}
