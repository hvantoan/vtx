import React from "react";
import { getStageProgress } from "../lib/dashboard";

const SEGMENTS = ["prepare", "code", "verify", "qa", "gate", "pr"];

interface StageProgressBarProps {
  state?: string;
  resumeState?: string | null;
}

export function StageProgressBar({ state, resumeState }: StageProgressBarProps) {
  const progress = getStageProgress(state, resumeState);
  const activeSegments = progress === null ? 0 : Math.ceil((progress / 100) * SEGMENTS.length);
  const overlay = state === "blocked" ? "blocked" : state === "retry_wait" ? "retry wait" : null;

  return (
    <div aria-label={`Stage progress ${progress === null ? "not available" : `${progress}%`}`}>
      <div className="grid grid-cols-6 gap-1" aria-hidden="true">
        {SEGMENTS.map((segment, index) => (
          <span
            key={segment}
            className={`h-1.5 rounded-sm ${index < activeSegments ? "bg-accent" : "bg-elevated"}`}
          />
        ))}
      </div>
      <div className="mt-1 flex items-center justify-between font-mono text-[11px] text-slate-500">
        <span>{overlay ?? "pipeline"}</span>
        <span>{progress === null ? "—" : `${progress}%`}</span>
      </div>
    </div>
  );
}
