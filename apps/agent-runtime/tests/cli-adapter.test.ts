import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { execFile } from "node:child_process";
import { rm, readFile, realpath } from "node:fs/promises";
import * as path from "node:path";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { AgentOutputSummary } from "../src/adapters/cli.js";
import { OmpCliAdapter } from "../src/adapters/omp.js";
import { PiCliAdapter } from "../src/adapters/pi.js";
import { AgentUnavailableError } from "../src/adapters/types.js";
import type { AdapterEvent } from "../src/adapters/types.js";
import type { TaskExecuteParams } from "../src/protocol/types.js";
import { makeTempDir } from "./support/host-harness.js";

const execFileAsync = promisify(execFile);
const FAKE_AGENT = path.join(path.dirname(fileURLToPath(import.meta.url)), "fixtures", "fake-agent.sh");

let worktree: string;

async function git(...args: string[]): Promise<string> {
  const { stdout } = await execFileAsync("git", args, { cwd: worktree });
  return stdout.trim();
}

beforeEach(async () => {
  worktree = await realpath(await makeTempDir("vtx-adapter-wt-"));
  await git("init", "-q", "-b", "main");
  await git("-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "base");
});

afterEach(async () => {
  await rm(worktree, { recursive: true, force: true });
});

function childEnv(extra: NodeJS.ProcessEnv = {}): NodeJS.ProcessEnv {
  return { PATH: hostPath(), HOME: worktree, ...extra };
}

function hostPath(): string {
  return process["env"].PATH ?? "/usr/bin:/bin";
}

function params(prompt: string): TaskExecuteParams {
  return { task_id: "run-1", prompt, workspace_path: worktree };
}

function collect() {
  const events: AdapterEvent[] = [];
  return { events, onEvent: (event: AdapterEvent) => events.push(event) };
}

describe("PiCliAdapter with a scripted fake agent binary", () => {
  it("is a real adapter (never a simulation) and detects the CLI version in preflight", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    expect(adapter.isSimulation).toBe(false);
    expect(adapter.version).toBe("unknown");
    await adapter.preflight(childEnv());
    expect(adapter.version).toBe("fake-agent 9.9.9");
  });

  it("fails preflight with AgentUnavailableError when the binary is missing", async () => {
    const adapter = new PiCliAdapter({ binary: path.join(worktree, "no-such-agent") });
    await expect(adapter.preflight(childEnv())).rejects.toBeInstanceOf(AgentUnavailableError);
  });

  it("runs in the worktree, streams events, and creates a commit that is reported as an artifact", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const { events, onEvent } = collect();
    const baseSha = await git("rev-parse", "HEAD");

    const result = await adapter.execute({
      params: params("implement the feature"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent,
      signal: new AbortController().signal,
    });

    expect(result.status).toBe("succeeded");
    expect(result.test_double).toBe(false);
    expect(result.output).toContain("done: committed agent-output.txt");

    const headSha = await git("rev-parse", "HEAD");
    expect(headSha).not.toBe(baseSha);
    expect(await git("log", "-1", "--format=%s")).toBe("feat: fake agent change");
    expect(await readFile(path.join(worktree, "agent-output.txt"), "utf8")).toBe("written by fake agent\n");
    expect(result.artifacts).toEqual([{ kind: "commit", ref: headSha }]);

    const types = events.map((e) => e.event_type);
    expect(types[0]).toBe("status_change");
    expect(types).toContain("tool_call");
    expect(events.find((e) => e.event_type === "tool_call")).toMatchObject({ payload: "write", data: { tool: "write" } });
    expect(events).toContainEqual({ event_type: "stdout", payload: "raw non-json noise line" });
    expect(events).toContainEqual({ event_type: "stdout", payload: "done: committed agent-output.txt" });
    expect(events.at(-1)).toEqual({ event_type: "status_change", payload: "completed" });
  });

  it("passes the task prompt, test baseline, and delivery instructions to the CLI", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const result = await adapter.execute({
      params: { ...params("MODE_ECHO_PROMPT implement X"), test_baseline: "12 tests pass" },
      workspacePath: worktree,
      env: childEnv(),
      onEvent: () => undefined,
      signal: new AbortController().signal,
    });
    expect(result.status).toBe("succeeded");
    expect(result.artifacts).toEqual([]);
    const received = await readFile(path.join(worktree, "received-prompt.txt"), "utf8");
    expect(received).toContain("MODE_ECHO_PROMPT implement X");
    expect(received).toContain("## Test baseline\n12 tests pass");
    expect(received).toContain("commit it with git");
  });

  it("runs the CLI with the worktree as its working directory", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const { events, onEvent } = collect();
    await adapter.execute({
      params: params("MODE_ENV"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent,
      signal: new AbortController().signal,
    });
    expect(events.some((e) => e.event_type === "stdout" && e.payload?.includes(`cwd=${worktree}`))).toBe(true);
  });

  it("gives the child only the env the host passes (no ambient secrets)", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const { events, onEvent } = collect();
    await adapter.execute({
      params: params("MODE_ENV"),
      workspacePath: worktree,
      env: childEnv({ FAKE_SECRET_TOKEN: "allowlisted-value" }),
      onEvent,
      signal: new AbortController().signal,
    });
    const line = events.find((e) => e.payload?.startsWith("leak="));
    expect(line?.payload).toContain("leak=allowlisted-value");
    expect(line?.payload).toContain("other=unset");
  });

  it("maps a non-zero exit to failed with the stderr tail and streams stderr events", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const { events, onEvent } = collect();
    const result = await adapter.execute({
      params: params("MODE_FAIL"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent,
      signal: new AbortController().signal,
    });
    expect(result.status).toBe("failed");
    expect(result.error).toContain("exit code 3");
    expect(result.error).toContain("fake agent exploded");
    expect(events).toContainEqual({ event_type: "stderr", payload: "fake agent exploded" });
  });

  it("returns failed (not a throw) when the CLI cannot be spawned at execute time", async () => {
    const adapter = new PiCliAdapter({ binary: path.join(worktree, "vanished-agent") });
    const result = await adapter.execute({
      params: params("anything"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent: () => undefined,
      signal: new AbortController().signal,
    });
    expect(result.status).toBe("failed");
    expect(result.error).toContain("failed to spawn pi CLI");
  });

  it("cancels a running agent with SIGTERM and returns cancelled with the abort reason", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const controller = new AbortController();
    const running = Promise.withResolvers<void>();
    const resultPromise = adapter.execute({
      params: params("MODE_HANG"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent: (event) => {
        if (event.payload === "hanging now") running.resolve();
      },
      signal: controller.signal,
    });
    await running.promise;
    controller.abort("operator_cancel");
    const result = await resultPromise;
    expect(result).toMatchObject({ status: "cancelled", error: "cancelled: operator_cancel" });
  });

  it("escalates to SIGKILL when the agent ignores SIGTERM", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT, killGraceMs: 50 });
    const controller = new AbortController();
    const running = Promise.withResolvers<void>();
    const resultPromise = adapter.execute({
      params: params("MODE_IGNORE_TERM"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent: (event) => {
        if (event.payload === "ignoring SIGTERM") running.resolve();
      },
      signal: controller.signal,
    });
    await running.promise;
    controller.abort("timeout");
    const result = await resultPromise;
    expect(result).toMatchObject({ status: "cancelled", error: "cancelled: timeout" });
  });

  it("returns cancelled immediately when the signal is already aborted", async () => {
    const adapter = new PiCliAdapter({ binary: FAKE_AGENT });
    const result = await adapter.execute({
      params: params("never runs"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent: () => undefined,
      signal: AbortSignal.abort("superseded"),
    });
    expect(result).toMatchObject({ status: "cancelled", error: "cancelled: superseded" });
    expect(await git("log", "--oneline")).not.toContain("fake agent");
  });
});

describe("OmpCliAdapter", () => {
  it("is a real secondary adapter named omp that shells out with --cwd", async () => {
    const adapter = new OmpCliAdapter({ binary: FAKE_AGENT });
    expect(adapter.name).toBe("omp");
    expect(adapter.isSimulation).toBe(false);
    const result = await adapter.execute({
      params: params("implement via omp"),
      workspacePath: worktree,
      env: childEnv(),
      onEvent: () => undefined,
      signal: new AbortController().signal,
    });
    expect(result.status).toBe("succeeded");
    expect(result.artifacts).toHaveLength(1);
  });
});

describe("AgentOutputSummary", () => {
  it("emits assistant text and tool calls, drops bookkeeping events, and passes raw lines through", () => {
    const summary = new AgentOutputSummary();
    expect(summary.consumeStdoutLine('{"type":"message_update","usage":{}}')).toEqual([]);
    expect(summary.consumeStdoutLine('{"type":"message_end","message":{"role":"user","content":"hi"}}')).toEqual([]);
    expect(
      summary.consumeStdoutLine('{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}')
    ).toEqual([{ event_type: "stdout", payload: "ab" }]);
    expect(summary.consumeStdoutLine('{"type":"tool_execution_start","toolName":"bash","args":{"command":"ls"}}')).toEqual([
      { event_type: "tool_call", payload: "bash", data: { tool: "bash", args: { command: "ls" } } },
    ]);
    expect(summary.consumeStdoutLine("plain text")).toEqual([{ event_type: "stdout", payload: "plain text" }]);
    expect(summary.output()).toBe("ab");
  });

  it("omits oversized tool args from structured data", () => {
    const summary = new AgentOutputSummary();
    const big = "x".repeat(20_000);
    const [event] = summary.consumeStdoutLine(JSON.stringify({ type: "tool_execution_start", toolName: "write", args: { big } }));
    expect(event?.data).toEqual({ tool: "write" });
  });
});
