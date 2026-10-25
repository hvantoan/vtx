import type { RunState } from "../types/api";

export type BadgeVariant = "success" | "warning" | "destructive" | "info" | "neutral";

export interface RunStatePresentation {
  label: string;
  variant: BadgeVariant;
}

const RUN_STATE_PRESENTATIONS: Record<RunState, RunStatePresentation> = {
  queued: { label: "QUEUED", variant: "warning" },
  preparing: { label: "PREPARING", variant: "info" },
  coding: { label: "CODING", variant: "info" },
  verifying: { label: "VERIFYING", variant: "info" },
  qa: { label: "QA", variant: "info" },
  quality_gate: { label: "QUALITY GATE", variant: "info" },
  pr_draft: { label: "PR DRAFT", variant: "info" },
  completed: { label: "COMPLETED", variant: "success" },
  blocked: { label: "BLOCKED", variant: "warning" },
  retry_wait: { label: "RETRY WAIT", variant: "warning" },
  failed: { label: "FAILED", variant: "destructive" },
  cancelled: { label: "CANCELLED", variant: "destructive" },
};

const ACTIVE_STAGES: RunState[] = [
  "preparing",
  "coding",
  "verifying",
  "qa",
  "quality_gate",
  "pr_draft",
];

export function isRunState(value: string | undefined): value is RunState {
  return value !== undefined && value in RUN_STATE_PRESENTATIONS;
}

export function getRunStatePresentation(state: string | undefined): RunStatePresentation {
  return isRunState(state)
    ? RUN_STATE_PRESENTATIONS[state]
    : { label: "UNKNOWN", variant: "neutral" };
}

export function getStageProgress(state: string | undefined, resumeState?: string | null): number | null {
  if (state === "completed") return 100;
  if (state === "queued") return 0;
  if (state === "failed" || state === "cancelled") return null;

  const effectiveState = state === "blocked" || state === "retry_wait" ? resumeState : state;
  const index = ACTIVE_STAGES.findIndex((stage) => stage === effectiveState);
  if (index < 0) return null;
  return Math.round(((index + 1) / ACTIVE_STAGES.length) * 100);
}

export function getQaStatus(state: string | undefined, gateDecision?: "pass" | "fail"): RunStatePresentation {
  if (gateDecision === "pass") return { label: "QA EVIDENCE ACCEPTED", variant: "success" };
  if (gateDecision === "fail") return { label: "QA EVIDENCE REJECTED", variant: "destructive" };
  if (state === "qa") return { label: "QA IN PROGRESS", variant: "info" };

  const passedQaStates = new Set(["quality_gate", "pr_draft", "completed"]);
  if (state && passedQaStates.has(state)) {
    return { label: "QA STAGE PASSED", variant: "success" };
  }

  return { label: "QA NOT MEASURED", variant: "neutral" };
}

export function formatDuration(seconds: number | undefined): string {
  if (seconds === undefined || !Number.isFinite(seconds)) return "—";
  const whole = Math.max(0, Math.floor(seconds));
  const hours = Math.floor(whole / 3600);
  const minutes = Math.floor((whole % 3600) / 60);
  const remainder = whole % 60;
  return [hours, minutes, remainder].map((part) => String(part).padStart(2, "0")).join(":");
}

export function formatLatency(milliseconds: number | null | undefined): string {
  if (milliseconds === null || milliseconds === undefined) return "—";
  return milliseconds >= 1000
    ? `${(milliseconds / 1000).toFixed(2)} s`
    : `${milliseconds.toFixed(1)} ms`;
}

export function formatTimestamp(value: string | undefined): string {
  if (!value) return "—";
  const date = new Date(value);
  return Number.isNaN(date.valueOf()) ? value : date.toISOString();
}

export function shortenIdentifier(value: string | null | undefined, length = 10): string {
  if (!value) return "—";
  return value.length > length ? value.slice(0, length) : value;
}

export function safeExternalUrl(value: string | null | undefined): string | null {
  if (!value) return null;
  try {
    const url = new URL(value);
    return url.protocol === "https:" || url.protocol === "http:" ? url.toString() : null;
  } catch {
    return null;
  }
}
