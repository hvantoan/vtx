import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { readFile, rm } from "node:fs/promises";
import * as path from "node:path";
import { FileSessionRegistry, InMemorySessionRegistry } from "../src/sessions/registry.js";
import type { AgentEventRecord, NewAgentSession, SessionRegistry } from "../src/sessions/registry.js";
import { makeTempDir } from "./support/host-harness.js";

const newSession: NewAgentSession = {
  run_id: "11111111-1111-4111-8111-111111111111",
  attempt: 1,
  agent_type: "pi",
  agent_version: "1.4.0",
  protocol_version: "1.1",
  is_test_double: false,
  status: "running",
  worktree_path: "/tmp/wt",
  pid: 42,
  exit_reason: null,
  error_code: null,
  error_message: null,
  started_at: "2026-01-01T00:00:00.000Z",
  finished_at: null,
};

function event(sessionId: string, seq: number): AgentEventRecord {
  return { session_id: sessionId, run_id: newSession.run_id, seq, event_type: "stdout", payload: `p${seq}`, data: null, event_ts: "2026-01-01T00:00:00.000Z" };
}

function behavesLikeAgentTables(make: () => SessionRegistry): void {
  it("begins a session idempotently on (run_id, attempt)", async () => {
    const registry = make();
    const first = await registry.beginSession(newSession);
    const again = await registry.beginSession(newSession);
    const retry = await registry.beginSession({ ...newSession, attempt: 2 });
    expect(again.session_id).toBe(first.session_id);
    expect(retry.session_id).not.toBe(first.session_id);
    expect(first).toMatchObject({ ...newSession });
    expect(first.created_at).toBe(first.updated_at);
  });

  it("applies status patches and keeps the agent_sessions column set", async () => {
    const registry = make();
    const row = await registry.beginSession(newSession);
    const updated = await registry.updateSession(row.session_id, { status: "failed", error_code: "-32003", error_message: "boom", finished_at: "2026-01-01T00:01:00.000Z" });
    expect(updated).toMatchObject({ status: "failed", error_code: "-32003", error_message: "boom" });
    expect((await registry.getSession(row.session_id))?.status).toBe("failed");
    expect(Object.keys(updated).sort()).toEqual(
      [
        "agent_type", "agent_version", "attempt", "created_at", "error_code", "error_message", "exit_reason", "finished_at",
        "is_test_double", "pid", "protocol_version", "run_id", "session_id", "started_at", "status", "updated_at", "worktree_path",
      ].sort()
    );
  });

  it("rejects updates to an unknown session", async () => {
    await expect(make().updateSession("missing", { status: "failed" })).rejects.toThrow(/not found/);
  });

  it("appends events idempotently on (session_id, seq) and lists them in seq order", async () => {
    const registry = make();
    const row = await registry.beginSession(newSession);
    await registry.appendEvent(event(row.session_id, 2));
    await registry.appendEvent(event(row.session_id, 1));
    await registry.appendEvent({ ...event(row.session_id, 2), payload: "duplicate-ignored" });
    const events = await registry.listEvents(row.session_id);
    expect(events.map((e) => [e.seq, e.payload])).toEqual([[1, "p1"], [2, "p2"]]);
  });
}

describe("InMemorySessionRegistry", () => {
  behavesLikeAgentTables(() => new InMemorySessionRegistry());
});

describe("FileSessionRegistry", () => {
  let dir: string;
  beforeEach(async () => {
    dir = await makeTempDir("vtx-registry-");
  });
  afterEach(async () => {
    await rm(dir, { recursive: true, force: true });
  });

  behavesLikeAgentTables(() => new FileSessionRegistry(dir));

  it("survives a restart by replaying the JSONL log", async () => {
    const first = new FileSessionRegistry(dir);
    const row = await first.beginSession(newSession);
    await first.updateSession(row.session_id, { status: "succeeded" });
    await first.appendEvent(event(row.session_id, 1));

    const restarted = new FileSessionRegistry(dir);
    expect((await restarted.getSession(row.session_id))?.status).toBe("succeeded");
    expect((await restarted.beginSession(newSession)).session_id).toBe(row.session_id);
    await restarted.appendEvent(event(row.session_id, 1));
    expect(await restarted.listEvents(row.session_id)).toHaveLength(1);
    const lines = (await readFile(path.join(dir, "sessions.jsonl"), "utf8")).trim().split("\n");
    expect(lines).toHaveLength(3);
  });
});
