import { appendFile, mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

import type { JiraIssue } from "./types.js";

export const STATE_FILE = "jira-state.json";
export const QUEUE_FILE = "jira-queue.jsonl";

export interface SeenIssue {
  key: string;
  version: string;
  summary: string;
  status: string;
  firstSeenAt: string;
  lastSeenAt: string;
}

export interface MasterState {
  version: 1;
  baselineDone: boolean;
  updatedAt: string;
  seen: Record<string, SeenIssue>;
}

export interface QueueEntry {
  discoveredAt: string;
  version: string;
  issue: JiraIssue;
}

export function statePath(stateDir: string): string {
  return join(stateDir, STATE_FILE);
}

export function queuePath(stateDir: string): string {
  return join(stateDir, QUEUE_FILE);
}

export function emptyState(): MasterState {
  return { version: 1, baselineDone: false, updatedAt: "", seen: {} };
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function parseState(raw: string): MasterState {
  const parsed: unknown = JSON.parse(raw);
  if (!isRecord(parsed) || parsed.version !== 1 || !isRecord(parsed.seen)) {
    throw new Error("corrupt master state: expected version 1 with a seen map");
  }
  const seen: Record<string, SeenIssue> = {};
  for (const [key, value] of Object.entries(parsed.seen)) {
    if (!isRecord(value) || typeof value.version !== "string") {
      throw new Error(`corrupt master state: invalid entry for ${key}`);
    }
    seen[key] = {
      key,
      version: value.version,
      summary: typeof value.summary === "string" ? value.summary : "",
      status: typeof value.status === "string" ? value.status : "",
      firstSeenAt: typeof value.firstSeenAt === "string" ? value.firstSeenAt : "",
      lastSeenAt: typeof value.lastSeenAt === "string" ? value.lastSeenAt : "",
    };
  }
  return {
    version: 1,
    baselineDone: parsed.baselineDone === true,
    updatedAt: typeof parsed.updatedAt === "string" ? parsed.updatedAt : "",
    seen,
  };
}

export async function loadState(path: string): Promise<MasterState> {
  try {
    return parseState(await readFile(path, "utf8"));
  } catch (error) {
    if (isRecord(error) && error.code === "ENOENT") return emptyState();
    throw error;
  }
}

export async function saveState(path: string, state: MasterState): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  const tmp = `${path}.tmp`;
  await writeFile(tmp, `${JSON.stringify(state, null, 2)}\n`, "utf8");
  await rename(tmp, path);
}

/**
 * At-least-once handoff: append before saving the seen cursor. A crash between
 * the two re-emits entries; the spine's idempotent intake is expected to absorb
 * that. Losing entries silently would be worse.
 */
export async function appendQueue(path: string, entries: readonly QueueEntry[]): Promise<void> {
  if (entries.length === 0) return;
  await mkdir(dirname(path), { recursive: true });
  await appendFile(path, `${entries.map((entry) => JSON.stringify(entry)).join("\n")}\n`, "utf8");
}
