import React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { RunStateBadge } from "../src/components/run-state-badge";
import { StageProgressBar } from "../src/components/stage-progress-bar";
import { getRunStatePresentation, getStageProgress } from "../src/lib/dashboard";
import type { RunState } from "../src/types/api";

const STATES: RunState[] = [
  "queued",
  "preparing",
  "coding",
  "verifying",
  "qa",
  "quality_gate",
  "pr_draft",
  "completed",
  "blocked",
  "retry_wait",
  "failed",
  "cancelled",
];

describe("operator status components", () => {
  it("maps every registry run state to a visible label and approved badge variant", () => {
    for (const state of STATES) {
      const presentation = getRunStatePresentation(state);
      expect(presentation.label).not.toBe("UNKNOWN");
      expect(["success", "warning", "destructive", "info", "neutral"]).toContain(presentation.variant);
      expect(renderToStaticMarkup(<RunStateBadge state={state} />)).toContain(presentation.label);
    }
  });

  it("renders honest stage progress including parked and terminal states", () => {
    expect(getStageProgress("queued")).toBe(0);
    expect(getStageProgress("preparing")).toBe(17);
    expect(getStageProgress("pr_draft")).toBe(100);
    expect(getStageProgress("completed")).toBe(100);
    expect(getStageProgress("blocked", "coding")).toBe(33);
    expect(getStageProgress("failed")).toBeNull();

    const parked = renderToStaticMarkup(<StageProgressBar state="blocked" resumeState="coding" />);
    expect(parked).toContain("blocked");
    expect(parked).toContain("33%");
  });

  it("renders unknown states as text, never color alone", () => {
    const markup = renderToStaticMarkup(<RunStateBadge state="future_state" />);
    expect(markup).toContain("UNKNOWN");
    expect(markup).toContain("role=\"status\"");
  });
});
