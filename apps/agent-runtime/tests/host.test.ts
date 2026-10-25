import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { mkdir, writeFile, rm } from "node:fs/promises";
import * as path from "node:path";
import { InMemorySessionRegistry } from "../src/sessions/registry.js";
import { createHostHarness, makeTempDir } from "./support/host-harness.js";
import { TestDoubleAdapter } from "./support/test-double-adapter.js";

let workspace: string;

beforeEach(async () => {
  workspace = await makeTempDir("vtx-host-ws-");
});

afterEach(async () => {
  vi.useRealTimers();
  await rm(workspace, { recursive: true, force: true });
});

function executeParams(overrides: Record<string, unknown> = {}): Record<string, unknown> {
  return { task_id: "task-1", prompt: "do the thing", workspace_path: workspace, ...overrides };
}

function pair(options: ConstructorParameters<typeof TestDoubleAdapter>[1] = {}) {
  const pi = new TestDoubleAdapter("pi", options);
  const omp = new TestDoubleAdapter("omp", options);
  return { pi, omp, harness: createHostHarness([pi, omp]) };
}

describe("initialize handshake", () => {
  it("negotiates v1.1 and reports TEST DOUBLE adapters honestly", async () => {
    const { harness } = pair();
    const resp = await harness.initialize({ agent_type: "pi" });
    expect(resp.error).toBeUndefined();
    expect(resp.result).toMatchObject({
      protocol_version: "1.1",
      agent_type: "pi",
      agent_version: "test-double-1.0.0",
      host: { runtime_pid: process.pid },
      capabilities: { streaming: true, cancellation: true, heartbeat_ms: 5000, max_event_bytes: 65536 },
    });
    expect(resp.result).toMatchObject({
      adapters: [
        { name: "pi", test_double: true },
        { name: "omp", test_double: true },
      ],
    });
  });

  it("selects the OMP adapter when requested", async () => {
    const { harness } = pair();
    const resp = await harness.initialize({ agent_type: "omp" });
    expect(resp.result).toMatchObject({ agent_type: "omp" });
  });

  it("negotiates down to 1.0 without heartbeat support", async () => {
    const { harness } = pair();
    const resp = await harness.initialize({ protocol_version: "1.0" });
    expect(resp.result).toMatchObject({ protocol_version: "1.0" });
    expect((resp.result as { capabilities: { heartbeat_ms?: number } }).capabilities.heartbeat_ms).toBeUndefined();
  });

  it("clamps a newer minor version to the host maximum", async () => {
    const { harness } = pair();
    const resp = await harness.initialize({ protocol_version: "1.7" });
    expect(resp.result).toMatchObject({ protocol_version: "1.1" });
  });

  it("rejects a major version mismatch with -32001", async () => {
    const { harness } = pair();
    const resp = await harness.initialize({ protocol_version: "2.0" });
    expect(resp.error?.code).toBe(-32001);
  });

  it("rejects invalid initialize params with -32602", async () => {
    const { harness } = pair();
    const resp = await harness.request(1, "initialize", { protocol_version: "banana", client_name: "x" });
    expect(resp.error?.code).toBe(-32602);
  });

  it("rejects heartbeat_ms below the contract minimum", async () => {
    const { harness } = pair();
    const resp = await harness.initialize({ capabilities: { heartbeat_ms: 10 } });
    expect(resp.error?.code).toBe(-32602);
  });

  it("rejects a second initialize with -32600", async () => {
    const { harness } = pair();
    await harness.initialize();
    const resp = await harness.request(2, "initialize", { protocol_version: "1.1", client_name: "x" });
    expect(resp.error?.code).toBe(-32600);
  });

  it("returns -32003 when the adapter preflight fails", async () => {
    const harness = createHostHarness([new TestDoubleAdapter("pi", { preflightError: "pi CLI missing" })]);
    const resp = await harness.initialize();
    expect(resp.error?.code).toBe(-32003);
    expect(resp.error?.message).toContain("pi CLI missing");
  });

  it("returns -32003 when no adapter is registered for the agent type", async () => {
    const harness = createHostHarness([new TestDoubleAdapter("pi")]);
    const resp = await harness.initialize({ agent_type: "omp" });
    expect(resp.error?.code).toBe(-32003);
  });
});

describe("task/execute streaming", () => {
  it("rejects task/execute before initialize", async () => {
    const { harness } = pair();
    const resp = await harness.request(5, "task/execute", executeParams());
    expect(resp.error?.code).toBe(-32600);
  });

  it("streams events with per-task seq starting at 1 and a terminal result echoing test_double", async () => {
    const { harness } = pair();
    await harness.initialize();

    const first = await harness.request(10, "task/execute", executeParams());
    expect(first.result).toMatchObject({ task_id: "task-1", status: "succeeded", test_double: true });

    const second = await harness.request(11, "task/execute", executeParams({ task_id: "task-2" }));
    expect(second.result).toMatchObject({ task_id: "task-2", status: "succeeded" });

    const seqFor = (taskId: string) => harness.events().filter((e) => e.task_id === taskId).map((e) => e.seq);
    expect(seqFor("task-1")).toEqual([1, 2, 3]);
    expect(seqFor("task-2")).toEqual([1, 2, 3]);
    const types = harness.events().map((e) => e.event_type);
    expect(types).toContain("status_change");
    expect(types).toContain("stdout");
    for (const event of harness.events()) expect(Date.parse(event.timestamp)).not.toBeNaN();
  });

  it("sends the terminal response after all of the task's events", async () => {
    const { harness } = pair();
    await harness.initialize();
    await harness.request(10, "task/execute", executeParams());
    const resultIndex = harness.messages.findIndex((m) => "id" in m && m.id === 10);
    const lastEventIndex = harness.messages.map((m) => "method" in m && m.method === "task/event").lastIndexOf(true);
    expect(resultIndex).toBeGreaterThan(lastEventIndex);
  });

  it("caps event payloads at 64 KiB", async () => {
    const huge = "x".repeat(200_000);
    const pi = new TestDoubleAdapter("pi", { events: [{ event_type: "stdout", payload: huge }] });
    const harness = createHostHarness([pi]);
    await harness.initialize();
    await harness.request(10, "task/execute", executeParams());
    const payload = harness.events()[0]?.payload ?? "";
    expect(Buffer.byteLength(payload)).toBeLessThanOrEqual(65536);
    expect(payload.endsWith("[truncated]")).toBe(true);
  });

  it("converts an adapter exception into a failed terminal result", async () => {
    const harness = createHostHarness([new TestDoubleAdapter("pi", { throwOnExecute: "boom" })]);
    await harness.initialize();
    const resp = await harness.request(10, "task/execute", executeParams());
    expect(resp.error).toBeUndefined();
    expect(resp.result).toMatchObject({ status: "failed", error: "boom", test_double: true });
  });
});

describe("task/execute validation", () => {
  it.each([
    ["missing prompt", { prompt: undefined }],
    ["missing workspace_path", { workspace_path: undefined }],
    ["empty task_id", { task_id: "" }],
    ["timeout_ms below minimum", { timeout_ms: 50 }],
    ["non-integer attempt", { attempt: 0 }],
    ["bad env_allowlist", { env_allowlist: [1] }],
  ])("returns -32602 for %s", async (_name, overrides) => {
    const { harness } = pair();
    await harness.initialize();
    const resp = await harness.request(10, "task/execute", executeParams(overrides));
    expect(resp.error?.code).toBe(-32602);
  });

  it("returns -32004 for a relative workspace path", async () => {
    const { harness } = pair();
    await harness.initialize();
    const resp = await harness.request(10, "task/execute", executeParams({ workspace_path: "relative/dir" }));
    expect(resp.error?.code).toBe(-32004);
  });

  it("returns -32004 for a missing directory", async () => {
    const { harness } = pair();
    await harness.initialize();
    const resp = await harness.request(10, "task/execute", executeParams({ workspace_path: path.join(workspace, "nope") }));
    expect(resp.error?.code).toBe(-32004);
  });

  it("returns -32004 when the workspace path is a file", async () => {
    const { harness } = pair();
    await harness.initialize();
    const file = path.join(workspace, "file.txt");
    await writeFile(file, "x");
    const resp = await harness.request(10, "task/execute", executeParams({ workspace_path: file }));
    expect(resp.error?.code).toBe(-32004);
  });

  it("frees the task slot after a workspace error", async () => {
    const { harness } = pair();
    await harness.initialize();
    await harness.request(10, "task/execute", executeParams({ workspace_path: "relative" }));
    const ok = await harness.request(11, "task/execute", executeParams());
    expect(ok.result).toMatchObject({ status: "succeeded" });
  });

  it("canonicalizes symlinked workspaces before handing them to the adapter", async () => {
    const real = path.join(workspace, "real");
    await mkdir(real);
    const { symlink, realpath } = await import("node:fs/promises");
    const link = path.join(workspace, "link");
    await symlink(real, link);
    const { pi, harness } = pair();
    await harness.initialize();
    await harness.request(10, "task/execute", executeParams({ workspace_path: link }));
    expect(pi.requests[0]?.workspacePath).toBe(await realpath(real));
  });
});

describe("single-task discipline", () => {
  it("rejects a duplicate task_id with -32600 and a different task with -32002", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", id: 10, method: "task/execute", params: executeParams() });
    await pi.started;

    const duplicate = await harness.request(11, "task/execute", executeParams());
    expect(duplicate.error?.code).toBe(-32600);
    const other = await harness.request(12, "task/execute", executeParams({ task_id: "task-other" }));
    expect(other.error?.code).toBe(-32002);

    pi.release();
    const terminal = await harness.waitForResponse(10);
    expect(terminal.result).toMatchObject({ status: "succeeded" });
  });
});

describe("cancellation", () => {
  it("cancels via task/cancel notification and still sends a terminal cancelled result", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", id: 20, method: "task/execute", params: executeParams() });
    await pi.started;
    harness.send({ jsonrpc: "2.0", method: "task/cancel", params: { task_id: "task-1", reason: "operator_cancel" } });

    const terminal = await harness.waitForResponse(20);
    expect(terminal.result).toMatchObject({ task_id: "task-1", status: "cancelled", error: "cancelled: operator_cancel" });
    expect(harness.messages.filter((m) => "id" in m && m.id === null)).toHaveLength(0);
  });

  it("accepts task/cancel sent as a request and acknowledges it", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", id: 20, method: "task/execute", params: executeParams() });
    await pi.started;
    const ack = await harness.request(21, "task/cancel", { task_id: "task-1", reason: "operator_block" });
    expect(ack.result).toMatchObject({ cancelled: true, task_id: "task-1" });
    const terminal = await harness.waitForResponse(20);
    expect(terminal.result).toMatchObject({ status: "cancelled", error: "cancelled: operator_block" });
  });

  it("answers cancel-by-request for an unknown task with -32000", async () => {
    const { harness } = pair();
    await harness.initialize();
    const resp = await harness.request(30, "task/cancel", { task_id: "ghost" });
    expect(resp.error?.code).toBe(-32000);
  });

  it("silently ignores task/cancel notifications for unknown tasks", async () => {
    const { harness } = pair();
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", method: "task/cancel", params: { task_id: "ghost" } });
    const ping = await harness.request(31, "no_such_method");
    expect(ping.error?.code).toBe(-32601);
    expect(harness.messages.filter((m) => "error" in m && m.id === null)).toHaveLength(0);
  });
});

describe("timeouts and heartbeat (fake timers)", () => {
  beforeEach(() => {
    vi.useFakeTimers({ toFake: ["setTimeout", "clearTimeout", "setInterval", "clearInterval"] });
  });

  it("aborts the task with reason timeout when timeout_ms elapses", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", id: 40, method: "task/execute", params: executeParams({ timeout_ms: 1000 }) });
    await pi.started;
    await vi.advanceTimersByTimeAsync(1000);
    const terminal = await harness.waitForResponse(40);
    expect(terminal.result).toMatchObject({ status: "cancelled", error: "cancelled: timeout" });
  });

  it("does not time out a task that finishes first", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", id: 41, method: "task/execute", params: executeParams({ timeout_ms: 5000 }) });
    await pi.started;
    pi.release();
    const terminal = await harness.waitForResponse(41);
    expect(terminal.result).toMatchObject({ status: "succeeded" });
    expect(vi.getTimerCount()).toBe(0);
  });

  it("emits host/heartbeat every heartbeat_ms while a task runs and stops afterwards", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize({ capabilities: { heartbeat_ms: 1000 } });
    harness.send({ jsonrpc: "2.0", id: 42, method: "task/execute", params: executeParams() });
    await pi.started;
    await vi.advanceTimersByTimeAsync(3000);
    const beats = harness.notifications("host/heartbeat");
    expect(beats).toHaveLength(3);
    expect(beats[0]?.params).toMatchObject({ task_id: "task-1", pid: process.pid, last_event_seq: 3 });

    pi.release();
    await harness.waitForResponse(42);
    await vi.advanceTimersByTimeAsync(5000);
    expect(harness.notifications("host/heartbeat")).toHaveLength(3);
  });

  it("emits no heartbeat when negotiated down to 1.0", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize({ protocol_version: "1.0" });
    harness.send({ jsonrpc: "2.0", id: 43, method: "task/execute", params: executeParams() });
    await pi.started;
    await vi.advanceTimersByTimeAsync(20_000);
    expect(harness.notifications("host/heartbeat")).toHaveLength(0);
    pi.release();
    await harness.waitForResponse(43);
  });
});

describe("malformed frames and envelope errors", () => {
  it("answers a corrupt line with -32700, logs host/log, and keeps processing", async () => {
    const { harness } = pair();
    harness.sendRaw("{not json");
    const parseError = await harness.waitForMessage((m) => "error" in m && m.id === null);
    expect(parseError).toMatchObject({ error: { code: -32700 } });
    await harness.waitForMessage((m) => "method" in m && m.method === "host/log");

    const resp = await harness.initialize();
    expect(resp.result).toMatchObject({ protocol_version: "1.1" });
  });

  it.each([
    ["a JSON number", "123"],
    ["a JSON array", "[1,2]"],
    ["null", "null"],
  ])("rejects %s with -32600", async (_name, line) => {
    const { harness } = pair();
    harness.sendRaw(line);
    const resp = await harness.waitForMessage((m) => "error" in m);
    expect(resp).toMatchObject({ id: null, error: { code: -32600 } });
  });

  it("rejects the wrong jsonrpc version with -32600 and echoes the id", async () => {
    const { harness } = pair();
    harness.send({ jsonrpc: "1.0", id: 7, method: "initialize" });
    const resp = await harness.waitForResponse(7);
    expect(resp.error?.code).toBe(-32600);
  });

  it("rejects non-object params with -32600", async () => {
    const { harness } = pair();
    harness.send({ jsonrpc: "2.0", id: 8, method: "initialize", params: [1] });
    const resp = await harness.waitForResponse(8);
    expect(resp.error?.code).toBe(-32600);
  });

  it("returns -32601 for unknown methods and never answers unknown notifications with an error", async () => {
    const { harness } = pair();
    harness.send({ jsonrpc: "2.0", method: "mystery/notify" });
    const resp = await harness.request(9, "mystery/request");
    expect(resp.error?.code).toBe(-32601);
    expect(harness.messages.filter((m) => "error" in m)).toHaveLength(1);
  });
});

describe("shutdown and exit", () => {
  it("delivers the terminal cancelled result before acknowledging shutdown, then exits on exit", async () => {
    const { pi, harness } = pair({ hold: true });
    await harness.initialize();
    harness.send({ jsonrpc: "2.0", id: 50, method: "task/execute", params: executeParams() });
    await pi.started;

    const shutdown = await harness.request(51, "shutdown", {});
    expect(shutdown.result).toEqual({});
    const terminalIndex = harness.messages.findIndex((m) => "id" in m && m.id === 50);
    const shutdownIndex = harness.messages.findIndex((m) => "id" in m && m.id === 51);
    expect(terminalIndex).toBeGreaterThanOrEqual(0);
    expect(terminalIndex).toBeLessThan(shutdownIndex);
    expect(harness.messages[terminalIndex]).toMatchObject({ result: { status: "cancelled", error: "cancelled: shutdown" } });

    expect(harness.exits).toEqual([]);
    harness.send({ jsonrpc: "2.0", method: "exit" });
    await vi.waitFor(() => expect(harness.exits).toEqual([0]));
  });

  it("refuses new tasks after shutdown", async () => {
    const { harness } = pair();
    await harness.initialize();
    await harness.request(60, "shutdown", {});
    const resp = await harness.request(61, "task/execute", executeParams());
    expect(resp.error?.code).toBe(-32600);
  });

  it("exits when stdin closes", async () => {
    const { harness } = pair();
    await harness.initialize();
    harness.input.end();
    await vi.waitFor(() => expect(harness.exits).toEqual([0]));
  });
});

describe("secrets and env allowlist", () => {
  it("passes only allowlisted env names to the adapter and redacts secret values from events and results", async () => {
    const secret = "sk-super-secret-value";
    const pi = new TestDoubleAdapter("pi", {
      events: [
        { event_type: "stdout", payload: `token is ${secret}`, data: { leaked: secret } },
        { event_type: "stderr", payload: "clean" },
      ],
    });
    const harness = createHostHarness([pi], {
      baseEnv: { PATH: "/usr/bin", HOME: "/home/x", API_TOKEN: secret, UNLISTED_VAR: "nope" },
    });
    await harness.initialize({ env_allowlist: ["API_TOKEN", "MISSING_VAR", "bad name!"] });
    const resp = await harness.request(70, "task/execute", executeParams());
    expect(resp.result).toMatchObject({ status: "succeeded" });

    expect(Object.keys(pi.requests[0]?.env ?? {}).sort()).toEqual(["API_TOKEN", "HOME", "PATH"]);
    expect(JSON.stringify(harness.messages)).not.toContain(secret);
    expect(harness.events()[0]).toMatchObject({ payload: "token is [REDACTED]", data: { leaked: "[REDACTED]" } });
  });
});

describe("session registry persistence", () => {
  it("persists the session row and idempotent event log in the agent_sessions/agent_events shape", async () => {
    const registry = new InMemorySessionRegistry();
    const harness = createHostHarness([new TestDoubleAdapter("pi")], { registry });
    const runId = "11111111-2222-3333-4444-555555555555";
    await harness.initialize({ run_id: runId });
    await harness.request(80, "task/execute", executeParams({ task_id: runId, attempt: 2 }));

    const row = await findSession(registry, runId, 2);
    expect(row).toMatchObject({
      run_id: runId,
      attempt: 2,
      agent_type: "pi",
      protocol_version: "1.1",
      is_test_double: true,
      status: "succeeded",
      pid: process.pid,
      error_message: null,
    });
    expect(row.finished_at).not.toBeNull();
    expect((await registry.listEvents(row.session_id)).map((e) => e.seq)).toEqual([1, 2, 3]);
  });

  it("records cancelled sessions with the cancel reason", async () => {
    const registry = new InMemorySessionRegistry();
    const pi = new TestDoubleAdapter("pi", { hold: true });
    const harness = createHostHarness([pi], { registry });
    const runId = "aaaaaaaa-2222-3333-4444-555555555555";
    await harness.initialize({ run_id: runId });
    harness.send({ jsonrpc: "2.0", id: 81, method: "task/execute", params: executeParams({ task_id: runId }) });
    await pi.started;
    harness.send({ jsonrpc: "2.0", method: "task/cancel", params: { task_id: runId, reason: "lease_lost" } });
    await harness.waitForResponse(81);
    const row = await findSession(registry, runId, 1);
    expect(row).toMatchObject({ status: "cancelled", exit_reason: "lease_lost" });
  });
});

async function findSession(registry: InMemorySessionRegistry, runId: string, attempt: number) {
  const row = await registry.listSessionsForRun(runId, attempt);
  if (!row) throw new Error(`no session for run ${runId} attempt ${attempt}`);
  return row;
}
