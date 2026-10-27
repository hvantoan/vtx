"use client";

import { LogOut } from "lucide-react";
import { usePathname, useRouter } from "next/navigation";
import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { fetchCurrentUser, logoutSession, SESSION_EXPIRED_EVENT } from "../lib/api";
import type { SessionUser } from "../lib/api";

interface SessionContextValue {
  user: SessionUser;
  logout: () => Promise<void>;
}

const SessionContext = createContext<SessionContextValue | null>(null);

export function useSession(): SessionContextValue {
  const context = useContext(SessionContext);
  if (!context) throw new Error("useSession must be used inside SessionProvider");
  return context;
}

type SessionState = "checking" | "ready" | "error";

function SessionNotice({ children }: { children: ReactNode }) {
  return (
    <div className="flex min-h-screen items-center justify-center bg-base px-4 text-slate-300">
      <div className="rounded-lg border border-line bg-surface px-6 py-5 text-sm">{children}</div>
    </div>
  );
}

/**
 * Resolves the server-side session before rendering the console. Any 401
 * carrying the API's "unauthorized" code sends the browser to /login with a
 * return path, so a LAN tab and a tunnel tab behave identically.
 */
export function SessionProvider({ children }: { children: ReactNode }) {
  const router = useRouter();
  const pathname = usePathname();
  const isLogin = pathname === "/login";
  const [user, setUser] = useState<SessionUser | null>(null);
  const [state, setState] = useState<SessionState>("checking");
  const [attempt, setAttempt] = useState(0);
  const redirecting = useRef(false);

  const redirectToLogin = useCallback(() => {
    if (isLogin || redirecting.current) return;
    redirecting.current = true;
    const next = pathname ? `?next=${encodeURIComponent(pathname)}` : "";
    router.replace(`/login${next}`);
  }, [isLogin, pathname, router]);

  useEffect(() => {
    if (isLogin) return;
    let cancelled = false;
    void fetchCurrentUser().then((result) => {
      if (cancelled) return;
      if (result.status === 401) {
        redirectToLogin();
        return;
      }
      if (!result.data) {
        setState("error");
        return;
      }
      setUser(result.data);
      setState("ready");
    });
    return () => {
      cancelled = true;
    };
  }, [isLogin, attempt, redirectToLogin]);

  useEffect(() => {
    const onExpired = () => redirectToLogin();
    window.addEventListener(SESSION_EXPIRED_EVENT, onExpired);
    return () => window.removeEventListener(SESSION_EXPIRED_EVENT, onExpired);
  }, [redirectToLogin]);

  const logout = useCallback(async () => {
    await logoutSession();
    setUser(null);
    setState("checking");
    router.replace("/login");
    router.refresh();
  }, [router]);

  if (isLogin) return <>{children}</>;
  if (state === "checking") return <SessionNotice>Checking session…</SessionNotice>;
  if (state === "error" || !user) {
    return (
      <SessionNotice>
        <p>The API is unreachable, so the session cannot be verified.</p>
        <button
          type="button"
          className="button-secondary focus-ring mt-3"
          onClick={() => {
            setState("checking");
            setAttempt((current) => current + 1);
          }}
        >
          Retry
        </button>
      </SessionNotice>
    );
  }

  return <SessionContext.Provider value={{ user, logout }}>{children}</SessionContext.Provider>;
}

export function SessionControls() {
  const { user, logout } = useSession();
  const [busy, setBusy] = useState(false);

  const signOut = () => {
    setBusy(true);
    void logout().finally(() => setBusy(false));
  };

  return (
    <div className="flex items-center gap-2">
      <span className="hidden font-mono text-[11px] text-slate-500 sm:inline" title="Signed in as">
        {user.username}
      </span>
      <button
        type="button"
        className="button-secondary focus-ring"
        onClick={signOut}
        disabled={busy}
        aria-label="Log out"
      >
        <LogOut aria-hidden className="h-3.5 w-3.5" />
        <span className="hidden sm:inline">{busy ? "Signing out…" : "Log out"}</span>
      </button>
    </div>
  );
}
