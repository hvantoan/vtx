import Link from "next/link";
import type { ReactNode } from "react";

interface AppShellProps {
  title: string;
  eyebrow?: string;
  children: ReactNode;
}

export function AppShell({ title, eyebrow = "Operator Console", children }: AppShellProps) {
  return (
    <div className="min-h-screen bg-base text-slate-100">
      <header className="sticky top-0 z-20 border-b border-line bg-surface/95 backdrop-blur">
        <div className="mx-auto flex h-14 max-w-7xl items-center gap-3 px-4 sm:px-6">
          <Link href="/" className="flex items-center gap-2.5 rounded-md focus-ring">
            <span className="flex h-7 w-7 items-center justify-center rounded-md bg-accent text-sm font-extrabold text-white">
              V
            </span>
            <span className="text-[15px] font-bold tracking-tight">VTX Platform</span>
          </Link>
          <span className="hidden font-mono text-[11px] uppercase tracking-wider text-slate-500 sm:inline">
            / {eyebrow}
          </span>
          <div className="flex-1" />
          <nav aria-label="Operator console" className="flex items-center gap-1 text-sm">
            <Link href="/" className="nav-link focus-ring">Overview</Link>
            <Link href="/runs" className="nav-link focus-ring">Runs</Link>
            <Link href="/settings" className="nav-link focus-ring">Settings</Link>
          </nav>
        </div>
      </header>
      <main className="mx-auto max-w-7xl space-y-4 px-4 py-6 sm:px-6">
        <div>
          <p className="text-[11px] font-semibold uppercase tracking-wider text-slate-500">{eyebrow}</p>
          <h1 className="mt-1 text-[28px] font-bold leading-9 text-slate-100">{title}</h1>
        </div>
        {children}
      </main>
    </div>
  );
}
