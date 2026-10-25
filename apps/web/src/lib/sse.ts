import type { PlatformEvent } from "../types/api";

export function nextReconnectDelay(attempt: number): number {
  return Math.min(30_000, 1_000 * 2 ** Math.max(0, attempt));
}

export interface ParsedSseBlock {
  event: PlatformEvent | null;
  id: string | null;
}

function isPlatformEvent(value: unknown): value is PlatformEvent {
  if (typeof value !== "object" || value === null) return false;
  return (
    "id" in value &&
    "type" in value &&
    "timestamp" in value &&
    "source" in value &&
    "data" in value &&
    typeof value.id === "string" &&
    typeof value.type === "string" &&
    typeof value.timestamp === "string" &&
    typeof value.source === "string" &&
    typeof value.data === "object" &&
    value.data !== null
  );
}

export function parseSseBlock(block: string): ParsedSseBlock {
  const lines = block.split(/\r?\n/);
  const data = lines
    .filter((line) => line.startsWith("data:"))
    .map((line) => line.slice(5).trimStart())
    .join("\n");
  const idLine = lines.find((line) => line.startsWith("id:"));
  const id = idLine ? idLine.slice(3).trim() : null;

  if (!data) return { event: null, id };

  try {
    const parsed: unknown = JSON.parse(data);
    if (!isPlatformEvent(parsed)) return { event: null, id };
    return { event: parsed, id: id || parsed.id };
  } catch {
    return { event: null, id };
  }
}
