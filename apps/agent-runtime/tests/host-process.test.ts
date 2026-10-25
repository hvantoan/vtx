import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { execFile, spawn } from "node:child_process";
import type { ChildProcessWithoutNullStreams } from "node:child_process";
import { readFile, realpath, rm } from "node:fs/promises";
import * as path from "node:path";
import * as readline from "node:readline";
import { fileURLToPath } from "node:url";
import { promisify } from "node:util";
import { FileSessionRegistry } from "../src/sessions/registry.js";
import type { AgentSessionRecord } from "../src/sessions/registry.js";
import type { JsonRpcNotification, JsonRpcResponse } from "../src/protocol/types.js";
import { makeTempDir } from "./support/host-harness.js";

const execFileAsync = promisify(execFile);
const here = path.dirname(fileURLToPath(import.meta.url));
const PACKAGE_DIR = path.join(here, "..");
const FAKE_AGENT = path.join(here, "fixtures", "fake-agent.sh");
const RUN_ID = "0b5e3f0a-1111-4222-8333-444455556666";

type Wire = JsonRpcResponse | JsonRpcNotification;

interface HostProcess {
  child: ChildProcessWithoutNullStreams;
  lines: string[];
  messages: Wire[];
  send: (message: unknown) => void;
  sendRaw: (line: string) => void;
  waitFor: (predicate: (m: Wire) => boolean) => Promise<Wire>;
  exited: Promise<number | null>;
}

let worktree: string;
let registryDir: string;
let running: HostProcess | undefined;

async function git(...args: string[]): Promise<string> {
  const { stdout } = await execFileAsync("git", args, { cwd: worktree });
  return stdout.trim();
}

function startHost(): HostProcess {
  const child = spawn(process.execPath, ["--import", "tsx", "src/host.ts"], {
    cwd: PACKAGE_DIR,
    env: {
      PATH: process["env"].PATH,
      HOME: worktree,
      VTX_PI_BIN: FAKE_AGENT,
      VTX_SESSION_REGISTRY_DIR: registryDir,
    },
    stdio: ["pipe", "pipe", "pipe"],
  });
  const lines: string[] = [];
  const messages: Wire[] = [];
  const waiters: Array<{ predicate: (m: Wire) => boolean; resolve: (m: Wire) => void }> = [];
  readline.createInterface({ input: child.stdout }).on("line", (line) => {
    lines.push(line);
    const message = JSON.parse(line) as Wire;
    messages.push(message);
    for (const waiter of [...waiters]) {
      if (waiter.predicate(message)) {
        waiters.splice(waiters.indexOf(waiter), 1);
        waiter.resolve(message);
      }
    }
  });
  const exited = new Promise<number | null>((resolve) => child.once("exit", (code) => resolve(code)));
  const host: HostProcess = {
    child,
    lines,
    messages,
    send: (message) => void child.stdin.write(JSON.stringify(message) + "\n"),
    sendRaw: (line) => void child.stdin.write(line + "\n"),
    waitFor: (predicate) => {
      const existing = messages.find(predicate);
      if (existing) return Promise.resolve(existing);
      const { promise, resolve } = Promise.withResolvers<Wire>();
      waiters.push({ predicate, resolve });
      return promise;
    },
    exited,
  };
  running = host;
  return host;
}

const responseTo =
  (id: number) =>
  (m: Wire): boolean =>
    "id" in m && m.id === id;

beforeEach(async () => {
  worktree = await realpath(await makeTempDir("vtx-host-proc-wt-"));
  registryDir = await makeTempDir("vtx-host-proc-reg-");
  await git("init", "-q", "-b", "main");
  await git("-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "base");
});

afterEach(async () => {
  if (running && running.child.exitCode === null) running.child.kill("SIGKILL");
  running = undefined;
  await rm(worktree, { recursive: true, force: true });
  await rm(registryDir, { recursive: true, force: true });
});

describe("agent-runtime host process over real stdio (fake agent binary)", () => {
  it("runs handshake → execute → commit in a temp git worktree → shutdown/exit, with a pure NDJSON stdout", async () => {
    const host = startHost();
    host.send({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1.1", client_name: "e2e", agent_type: "pi", run_id: RUN_ID } });
    const init = (await host.waitFor(responseTo(1))) as JsonRpcResponse;
    expect(init.result).toMatchObject({ protocol_version: "1.1", agent_type: "pi", agent_version: "fake-agent 9.9.9" });
    expect(init.result).toMatchObject({ adapters: [{ name: "pi", test_double: false }, { name: "omp", test_double: false }] });

    const baseSha = await git("rev-parse", "HEAD");
    host.send({ jsonrpc: "2.0", id: 2, method: "task/execute", params: { task_id: RUN_ID, attempt: 1, prompt: "add a file", workspace_path: worktree } });
    const done = (await host.waitFor(responseTo(2))) as JsonRpcResponse;

    const headSha = await git("rev-parse", "HEAD");
    expect(headSha).not.toBe(baseSha);
    expect(done.result).toMatchObject({ task_id: RUN_ID, status: "succeeded", test_double: false, artifacts: [{ kind: "commit", ref: headSha }] });
    expect(await readFile(path.join(worktree, "agent-output.txt"), "utf8")).toBe("written by fake agent\n");

    const events = host.messages.filter((m): m is JsonRpcNotification => "method" in m && m.method === "task/event");
    const seqs = events.map((e) => (e.params as { seq: number }).seq);
    expect(seqs.length).toBeGreaterThan(3);
    expect(seqs).toEqual(seqs.map((_, i) => i + 1));

    host.send({ jsonrpc: "2.0", id: 3, method: "shutdown", params: {} });
    await host.waitFor(responseTo(3));
    host.send({ jsonrpc: "2.0", method: "exit" });
    expect(await host.exited).toBe(0);

    // stdout is protocol only: every line already parsed as a JSON-RPC 2.0 envelope.
    for (const message of host.messages) expect(message.jsonrpc).toBe("2.0");

    const registry = new FileSessionRegistry(registryDir);
    const sessions = await findSessions(registryDir, RUN_ID);
    expect(sessions).toMatchObject({ run_id: RUN_ID, attempt: 1, agent_type: "pi", is_test_double: false, status: "succeeded" });
    const persisted = await registry.listEvents(sessions.session_id);
    expect(persisted.map((e) => e.seq)).toEqual(seqs);
  });

  it("survives a malformed frame mid-stream and keeps serving requests", async () => {
    const host = startHost();
    host.sendRaw("this is not json");
    await host.waitFor((m) => "error" in m && m.id === null);
    host.send({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1.1", client_name: "e2e" } });
    const init = (await host.waitFor(responseTo(1))) as JsonRpcResponse;
    expect(init.error).toBeUndefined();
  });

  it("cancels a hanging real child process and returns a terminal cancelled result", async () => {
    const host = startHost();
    host.send({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1.1", client_name: "e2e" } });
    await host.waitFor(responseTo(1));
    host.send({ jsonrpc: "2.0", id: 2, method: "task/execute", params: { task_id: "t-hang", prompt: "MODE_HANG", workspace_path: worktree } });
    await host.waitFor((m) => "method" in m && m.method === "task/event" && (m.params as { payload?: string }).payload === "hanging now");
    host.send({ jsonrpc: "2.0", method: "task/cancel", params: { task_id: "t-hang", reason: "operator_cancel" } });
    const terminal = (await host.waitFor(responseTo(2))) as JsonRpcResponse;
    expect(terminal.result).toMatchObject({ status: "cancelled", error: "cancelled: operator_cancel" });
  });

  it("exits cleanly when the supervisor closes stdin", async () => {
    const host = startHost();
    host.send({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1.1", client_name: "e2e" } });
    await host.waitFor(responseTo(1));
    host.child.stdin.end();
    expect(await host.exited).toBe(0);
  });

  it("answers initialize with -32003 when the agent CLI is unavailable", async () => {
    const child = spawn(process.execPath, ["--import", "tsx", "src/host.ts"], {
      cwd: PACKAGE_DIR,
      env: { PATH: process["env"].PATH, HOME: worktree, VTX_PI_BIN: path.join(worktree, "missing-pi") },
      stdio: ["pipe", "pipe", "pipe"],
    });
    const { promise, resolve } = Promise.withResolvers<JsonRpcResponse>();
    readline.createInterface({ input: child.stdout }).on("line", (line) => resolve(JSON.parse(line) as JsonRpcResponse));
    child.stdin.write(JSON.stringify({ jsonrpc: "2.0", id: 1, method: "initialize", params: { protocol_version: "1.1", client_name: "e2e" } }) + "\n");
    const resp = await promise;
    child.kill("SIGKILL");
    expect(resp.error?.code).toBe(-32003);
  });
});

async function findSessions(dir: string, runId: string): Promise<AgentSessionRecord> {
  const raw = await readFile(path.join(dir, "sessions.jsonl"), "utf8");
  let latest: AgentSessionRecord | undefined;
  for (const line of raw.split("\n")) {
    if (!line) continue;
    const entry = JSON.parse(line) as { op: string; session?: AgentSessionRecord };
    if (entry.session?.run_id === runId) latest = entry.session;
  }
  if (!latest) throw new Error(`no persisted session for ${runId}`);
  return latest;
}
