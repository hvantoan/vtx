import { appendFile, mkdir, readFile } from "node:fs/promises";
import * as path from "node:path";
import type { AgentType, TaskEventType } from "../protocol/types.js";

export type SessionStatus =
  | "starting"
  | "ready"
  | "running"
  | "succeeded"
  | "failed"
  | "cancelled"
  | "crashed"
  | "killed"
  | "agent_unavailable";

/** Row shape of `agent_sessions` (apps/backend/db/migrations/00007_agent_runtime.sql). Timestamps are ISO-8601. */
export interface AgentSessionRecord {
  session_id: string;
  run_id: string;
  attempt: number;
  agent_type: AgentType;
  agent_version: string | null;
  protocol_version: string;
  is_test_double: boolean;
  status: SessionStatus;
  worktree_path: string;
  pid: number | null;
  exit_reason: string | null;
  error_code: string | null;
  error_message: string | null;
  started_at: string | null;
  finished_at: string | null;
  created_at: string;
  updated_at: string;
}

/** Row shape of `agent_events`; `(session_id, seq)` is the idempotency key. */
export interface AgentEventRecord {
  session_id: string;
  run_id: string;
  seq: number;
  event_type: TaskEventType;
  payload: string;
  data: Record<string, unknown> | null;
  event_ts: string;
}

export type NewAgentSession = Omit<AgentSessionRecord, "session_id" | "created_at" | "updated_at">;
export type SessionPatch = Partial<
  Pick<
    AgentSessionRecord,
    "status" | "pid" | "exit_reason" | "error_code" | "error_message" | "started_at" | "finished_at" | "agent_version"
  >
>;

export interface SessionRegistry {
  /** Idempotent on `(run_id, attempt)`: a repeated begin returns the existing row. */
  beginSession(session: NewAgentSession): Promise<AgentSessionRecord>;
  updateSession(sessionId: string, patch: SessionPatch): Promise<AgentSessionRecord>;
  /** Idempotent on `(session_id, seq)`: duplicates are ignored. */
  appendEvent(event: AgentEventRecord): Promise<void>;
  getSession(sessionId: string): Promise<AgentSessionRecord | undefined>;
  listEvents(sessionId: string): Promise<AgentEventRecord[]>;
}

export class InMemorySessionRegistry implements SessionRegistry {
  private sessions = new Map<string, AgentSessionRecord>();
  private events = new Map<string, Map<number, AgentEventRecord>>();

  async beginSession(session: NewAgentSession): Promise<AgentSessionRecord> {
    const existing = await this.listSessionsForRun(session.run_id, session.attempt);
    if (existing) return existing;
    const now = new Date().toISOString();
    const record: AgentSessionRecord = { ...session, session_id: crypto.randomUUID(), created_at: now, updated_at: now };
    this.sessions.set(record.session_id, record);
    return { ...record };
  }

  async updateSession(sessionId: string, patch: SessionPatch): Promise<AgentSessionRecord> {
    const existing = this.sessions.get(sessionId);
    if (!existing) throw new Error(`agent session ${sessionId} not found`);
    const updated: AgentSessionRecord = { ...existing, ...patch, updated_at: new Date().toISOString() };
    this.sessions.set(sessionId, updated);
    return { ...updated };
  }

  async appendEvent(event: AgentEventRecord): Promise<void> {
    if (!this.hasEvent(event.session_id, event.seq)) this.replay({ op: "event", event });
  }

  /** Existing row for `(run_id, attempt)`, if any. */
  async listSessionsForRun(runId: string, attempt: number): Promise<AgentSessionRecord | undefined> {
    for (const existing of this.sessions.values()) {
      if (existing.run_id === runId && existing.attempt === attempt) return { ...existing };
    }
    return undefined;
  }

  hasEvent(sessionId: string, seq: number): boolean {
    return this.events.get(sessionId)?.has(seq) ?? false;
  }

  /** Applies a persisted log entry verbatim (used to rebuild state from the JSONL file). */
  replay(entry: { op: "begin" | "update"; session: AgentSessionRecord } | { op: "event"; event: AgentEventRecord }): void {
    if (entry.op === "event") {
      let bySeq = this.events.get(entry.event.session_id);
      if (!bySeq) {
        bySeq = new Map();
        this.events.set(entry.event.session_id, bySeq);
      }
      bySeq.set(entry.event.seq, entry.event);
    } else {
      this.sessions.set(entry.session.session_id, entry.session);
    }
  }

  async getSession(sessionId: string): Promise<AgentSessionRecord | undefined> {
    const record = this.sessions.get(sessionId);
    return record ? { ...record } : undefined;
  }

  async listEvents(sessionId: string): Promise<AgentEventRecord[]> {
    return [...(this.events.get(sessionId)?.values() ?? [])].sort((a, b) => a.seq - b.seq);
  }
}

type SessionLogEntry =
  | { op: "begin" | "update"; session: AgentSessionRecord }
  | { op: "event"; event: AgentEventRecord };

/**
 * Append-only JSONL persistence (`<dir>/sessions.jsonl`) with the same row shapes as the
 * Postgres tables. The log is replayed once on first use and then served from memory,
 * so state survives host restarts. Single writer per directory is assumed.
 */
export class FileSessionRegistry implements SessionRegistry {
  private readonly file: string;
  private cache: InMemorySessionRegistry | undefined;
  private chain: Promise<unknown> = Promise.resolve();

  constructor(dir: string) {
    this.file = path.join(dir, "sessions.jsonl");
  }

  beginSession(session: NewAgentSession): Promise<AgentSessionRecord> {
    return this.serialized(async (state) => {
      const before = await state.listSessionsForRun(session.run_id, session.attempt);
      if (before) return before;
      const record = await state.beginSession(session);
      await this.append({ op: "begin", session: record });
      return record;
    });
  }

  updateSession(sessionId: string, patch: SessionPatch): Promise<AgentSessionRecord> {
    return this.serialized(async (state) => {
      const record = await state.updateSession(sessionId, patch);
      await this.append({ op: "update", session: record });
      return record;
    });
  }

  appendEvent(event: AgentEventRecord): Promise<void> {
    return this.serialized(async (state) => {
      if (state.hasEvent(event.session_id, event.seq)) return;
      await state.appendEvent(event);
      await this.append({ op: "event", event });
    });
  }

  getSession(sessionId: string): Promise<AgentSessionRecord | undefined> {
    return this.serialized((state) => state.getSession(sessionId));
  }

  listEvents(sessionId: string): Promise<AgentEventRecord[]> {
    return this.serialized((state) => state.listEvents(sessionId));
  }

  private serialized<T>(op: (state: InMemorySessionRegistry) => Promise<T>): Promise<T> {
    const run = async (): Promise<T> => op(await this.state());
    const next = this.chain.then(run, run);
    this.chain = next.catch(() => undefined);
    return next;
  }

  private async append(entry: SessionLogEntry): Promise<void> {
    await mkdir(path.dirname(this.file), { recursive: true });
    await appendFile(this.file, JSON.stringify(entry) + "\n");
  }

  private async state(): Promise<InMemorySessionRegistry> {
    if (this.cache) return this.cache;
    const state = new InMemorySessionRegistry();
    let raw = "";
    try {
      raw = await readFile(this.file, "utf8");
    } catch (err: unknown) {
      if (!(err instanceof Error && "code" in err && err.code === "ENOENT")) throw err;
    }
    for (const line of raw.split("\n")) {
      if (line) state.replay(JSON.parse(line) as SessionLogEntry);
    }
    this.cache = state;
    return state;
  }
}
