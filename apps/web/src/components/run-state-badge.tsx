import React from "react";
import { getRunStatePresentation } from "../lib/dashboard";
import { MetricBadge } from "./metric-badge";

interface RunStateBadgeProps {
  state?: string;
}

export function RunStateBadge({ state }: RunStateBadgeProps) {
  const presentation = getRunStatePresentation(state);
  return <MetricBadge label={presentation.label} variant={presentation.variant} />;
}
