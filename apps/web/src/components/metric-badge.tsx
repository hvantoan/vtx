import React from "react";
import type { BadgeVariant } from "../lib/dashboard";

const VARIANT_CLASSES: Record<BadgeVariant, string> = {
  success: "border-success/30 bg-success/10 text-success",
  warning: "border-warning/30 bg-warning/10 text-warning",
  destructive: "border-danger/30 bg-danger/10 text-danger",
  info: "border-accent/30 bg-accent/10 text-accent",
  neutral: "border-slate-500/30 bg-slate-500/10 text-slate-400",
};

interface MetricBadgeProps {
  label: string;
  variant: BadgeVariant;
  size?: "sm" | "md";
  pulse?: boolean;
}

export function MetricBadge({ label, variant, size = "sm", pulse = false }: MetricBadgeProps) {
  return (
    <span
      role="status"
      className={`inline-flex items-center gap-1.5 rounded-md border px-2 py-0.5 font-semibold uppercase tracking-wider ${
        size === "md" ? "text-xs" : "text-[11px]"
      } ${VARIANT_CLASSES[variant]}`}
    >
      <span
        aria-hidden="true"
        className={`h-1.5 w-1.5 rounded-full bg-current ${pulse ? "status-pulse" : ""}`}
      />
      {label}
    </span>
  );
}
