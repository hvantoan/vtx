import { mkdir, readFile, rename, writeFile } from "node:fs/promises";
import { dirname, join } from "node:path";

import { SpineTool } from "../orchestrator/protocol.js";
import { SpineClient } from "../orchestrator/spine-client.js";
import type { QueueEntry } from "./state.js";

export const QUEUE_CURSOR_FILE = "jira-queue.cursor.json";

export interface QueueCursor {
  version: 1;
  /** Number of complete queue lines already delivered. */
  nextLine: number;
  updatedAt: string;
}

export interface IntakeOutcome {
  disposition: string;
  issueKey: string;
  intentId?: string;
  reasonCode?: string;
}

export type Deliver = (entry: QueueEntry) => Promise<IntakeOutcome>;

export interface DrainOptions {
  queuePath: string;
  cursorPath: string;
  deliver: Deliver;
  log: (event: Record<string, unknown>) => void;
  now: () => Date;
}

export interface DrainResult {
  delivered: number;
  failed: number;
  remaining: number;
}

export function queueCursorPath(stateDir: string): string {
  return join(stateDir, QUEUE_CURSOR_FILE);
}

export function emptyCursor(): QueueCursor {
  return { version: 1, nextLine: 0, updatedAt: "" };
}

function asRecord(value: unknown): Record<string, unknown> | undefined {
  if (typeof value !== "object" || value === null || Array.isArray(value)) return undefined;
  return value as Record<string, unknown>;
}

function parseEntry(line: string): QueueEntry | undefined {
  let parsed: unknown;
  try {
    parsed = JSON.parse(line);
  } catch {
    return undefined;
  }
  const entry = asRecord(parsed);
  const issue = asRecord(entry?.issue);
  if (typeof entry?.discoveredAt !== "string" || typeof entry?.version !== "string") return undefined;
  if (typeof issue?.id !== "string" || typeof issue?.key !== "string") return undefined;
  return parsed as QueueEntry;
}

async function loadCursor(path: string): Promise<QueueCursor> {
  try {
    const raw = await readFile(path, "utf8");
    const parsed = asRecord(JSON.parse(raw));
    if (parsed?.version !== 1 || typeof parsed.nextLine !== "number" || parsed.nextLine < 0) {
      throw new Error(`corrupt queue cursor at ${path}`);
    }
    return { version: 1, nextLine: Math.trunc(parsed.nextLine), updatedAt: typeof parsed.updatedAt === "string" ? parsed.updatedAt : "" };
  } catch (error) {
    if (asRecord(error)?.code === "ENOENT") return emptyCursor();
    throw error;
  }
}

async function saveCursor(path: string, cursor: QueueCursor): Promise<void> {
  await mkdir(dirname(path), { recursive: true });
  const tmp = `${path}.tmp`;
  await writeFile(tmp, `${JSON.stringify(cursor, null, 2)}\n`, "utf8");
  await rename(tmp, path);
}

/**
 * Deliver queued entries in order, advancing the cursor only after success.
 * A failure stops the drain and leaves the cursor on the failed line, so the
 * next tick retries it — the queue is the durable handoff, not the log.
 */
export async function drainQueue(options: DrainOptions): Promise<DrainResult> {
  let raw: string;
  try {
    raw = await readFile(options.queuePath, "utf8");
  } catch (error) {
    if (asRecord(error)?.code === "ENOENT") return { delivered: 0, failed: 0, remaining: 0 };
    throw error;
  }
  const lines = raw.split("\n").filter((line) => line.trim().length > 0);
  const cursor = await loadCursor(options.cursorPath);
  let index = Math.min(cursor.nextLine, lines.length);
  let delivered = 0;

  for (; index < lines.length; index += 1) {
    const entry = parseEntry(lines[index]);
    if (!entry) {
      options.log({ event: "jira_queue_corrupt", line: index + 1 });
      await saveCursor(options.cursorPath, { version: 1, nextLine: index + 1, updatedAt: options.now().toISOString() });
      continue;
    }
    try {
      const outcome = await options.deliver(entry);
      delivered += 1;
      options.log({
        event: "jira_intake",
        issue_key: outcome.issueKey,
        disposition: outcome.disposition,
        intent_id: outcome.intentId ?? "",
        reason_code: outcome.reasonCode ?? "",
        version: entry.version,
      });
    } catch (error) {
      options.log({
        event: "jira_intake_error",
        issue_key: entry.issue.key,
        error: error instanceof Error ? error.message : String(error),
        remaining: lines.length - index,
      });
      await saveCursor(options.cursorPath, { version: 1, nextLine: index, updatedAt: options.now().toISOString() });
      return { delivered, failed: 1, remaining: lines.length - index };
    }
    await saveCursor(options.cursorPath, { version: 1, nextLine: index + 1, updatedAt: options.now().toISOString() });
  }

  return { delivered, failed: 0, remaining: 0 };
}

/** The subset of SpineClient the intake deliverer needs (fakeable in tests). */
export interface SpineCaller {
  call(tool: string, params: { runId?: string; leaseEpoch?: number; args?: unknown }): Promise<unknown>;
  close(): void;
}

export function spineIntakeDeliver(client: SpineCaller): Deliver {
  return async (entry) => {
    const result = asRecord(
      await client.call(SpineTool.Intake, {
        args: {
          issue_id: entry.issue.id,
          issue_key: entry.issue.key,
          version: entry.version,
          correlation_id: `poll:${entry.issue.key}:${entry.version}`,
        },
      }),
    );
    const disposition = typeof result?.disposition === "string" ? result.disposition : "";
    if (disposition.length === 0) throw new Error("spine intake returned no disposition");
    return {
      disposition,
      issueKey: typeof result?.issue_key === "string" ? result.issue_key : entry.issue.key,
      intentId: typeof result?.intent_id === "string" ? result.intent_id : undefined,
      reasonCode: typeof result?.reason_code === "string" ? result.reason_code : undefined,
    };
  };
}

/** One connection per drain keeps the master loop free of stale-socket state. */
export async function connectSpine(socketPath: string, token: string): Promise<SpineClient> {
  const client = new SpineClient({ socketPath, token });
  await client.connect();
  return client;
}
