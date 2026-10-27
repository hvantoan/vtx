import { execFile, spawn } from "node:child_process";
import * as readline from "node:readline";
import { promisify } from "node:util";
import { logStderr } from "../protocol/framing.js";
import { isRecord } from "../protocol/guards.js";
import { MAX_EVENT_BYTES } from "../protocol/types.js";
import type { AgentType, TaskArtifact, TaskExecuteResult } from "../protocol/types.js";
import { AgentUnavailableError } from "./types.js";
import type { AdapterEvent, AdapterRequest, AgentAdapter } from "./types.js";

const execFileAsync = promisify(execFile);

const PREFLIGHT_TIMEOUT_MS = 10_000;
const DEFAULT_KILL_GRACE_MS = 3_000;
const MAX_TOOL_ARGS_BYTES = 8 * 1024;

export interface CliAdapterOptions {
  /** Absolute path or PATH-resolvable name of the agent CLI. */
  binary: string;
  /** Extra CLI flags inserted before the prompt (e.g. `--model`). */
  extraArgs?: string[];
  /** SIGTERM → SIGKILL escalation delay on cancel. Spec default: 3000 ms. */
  killGraceMs?: number;
}

/**
 * Real agent adapter: runs an agent CLI as a child process in the run's git worktree
 * and translates its `--mode json` event stream into `task/event` notifications.
 * Subclasses only differ in binary name and argument vector. `isSimulation` is always false.
 */
export abstract class CliAgentAdapter implements AgentAdapter {
  abstract readonly name: AgentType;
  readonly isSimulation = false;
  private detectedVersion = "unknown";

  protected readonly binary: string;
  private readonly extraArgs: string[];
  private readonly killGraceMs: number;

  constructor(options: CliAdapterOptions) {
    this.binary = options.binary;
    this.extraArgs = options.extraArgs ?? [];
    this.killGraceMs = options.killGraceMs ?? DEFAULT_KILL_GRACE_MS;
  }

  get version(): string {
    return this.detectedVersion;
  }

  /** Argument vector placed after `extraArgs`; must run non-interactively and emit NDJSON events on stdout. */
  protected abstract buildArgs(prompt: string, workspacePath: string): string[];

  async preflight(env: NodeJS.ProcessEnv): Promise<void> {
    try {
      const { stdout } = await execFileAsync(this.binary, ["--version"], {
        env,
        timeout: PREFLIGHT_TIMEOUT_MS,
      });
      const version = stdout.trim().split("\n")[0]?.trim();
      if (version) this.detectedVersion = version;
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err);
      throw new AgentUnavailableError(`${this.name} CLI "${this.binary}" failed preflight: ${message}`);
    }
  }

  async execute(request: AdapterRequest): Promise<TaskExecuteResult> {
    const { params, workspacePath, env, onEvent, signal } = request;
    const startedAt = Date.now();
    if (signal.aborted) return cancelledResult(params.task_id, signal, startedAt);

    const headBefore = await gitHead(workspacePath, env);
    const prompt = composePrompt(params.prompt, params.test_baseline);
    const args = [...this.extraArgs, ...this.buildArgs(prompt, workspacePath)];
    logStderr("Spawning agent CLI", { agent: this.name, taskId: params.task_id, cwd: workspacePath });

    const child = spawn(this.binary, args, { cwd: workspacePath, env, stdio: ["ignore", "pipe", "pipe"] });
    const exit = Promise.withResolvers<{ code: number | null; signal: NodeJS.Signals | null } | Error>();
    child.once("error", (err) => exit.resolve(err));
    child.once("close", (code, sig) => exit.resolve({ code, signal: sig }));

    let killTimer: NodeJS.Timeout | undefined;
    const onAbort = (): void => {
      child.kill("SIGTERM");
      killTimer = setTimeout(() => child.kill("SIGKILL"), this.killGraceMs);
    };
    signal.addEventListener("abort", onAbort, { once: true });
    if (signal.aborted) onAbort();

    onEvent({ event_type: "status_change", payload: "started" });

    const summary = new AgentOutputSummary();
    const stdoutDone = pumpLines(child.stdout, (line) => {
      const events = summary.consumeStdoutLine(line);
      for (const event of events) onEvent(event);
    });
    const stderrTail: string[] = [];
    const stderrDone = pumpLines(child.stderr, (line) => {
      stderrTail.push(line);
      if (stderrTail.length > 20) stderrTail.shift();
      onEvent({ event_type: "stderr", payload: line });
    });

    const outcome = await exit.promise;
    await Promise.all([stdoutDone, stderrDone]);
    clearTimeout(killTimer);
    signal.removeEventListener("abort", onAbort);

    if (outcome instanceof Error) {
      return {
        task_id: params.task_id,
        status: "failed",
        error: `failed to spawn ${this.name} CLI: ${outcome.message}`,
        test_double: false,
      };
    }
    if (signal.aborted) return cancelledResult(params.task_id, signal, startedAt);

    const usage = { duration_ms: Date.now() - startedAt };
    if (outcome.code !== 0) {
      const reason = outcome.signal ? `signal ${outcome.signal}` : `exit code ${outcome.code}`;
      const tail = stderrTail.join("\n");
      return {
        task_id: params.task_id,
        status: "failed",
        output: summary.output(),
        error: tail ? `${this.name} CLI exited with ${reason}: ${tail}` : `${this.name} CLI exited with ${reason}`,
        test_double: false,
        usage,
      };
    }

    onEvent({ event_type: "status_change", payload: "completed" });
    const artifacts: TaskArtifact[] = [];
    const headAfter = await gitHead(workspacePath, env);
    if (headAfter !== undefined && headAfter !== headBefore) artifacts.push({ kind: "commit", ref: headAfter });
    return {
      task_id: params.task_id,
      status: "succeeded",
      output: summary.output(),
      test_double: false,
      artifacts,
      usage,
    };
  }
}

function cancelledResult(taskId: string, signal: AbortSignal, startedAt: number): TaskExecuteResult {
  const reason = typeof signal.reason === "string" ? signal.reason : "cancelled";
  return {
    task_id: taskId,
    status: "cancelled",
    error: `cancelled: ${reason}`,
    test_double: false,
    usage: { duration_ms: Date.now() - startedAt },
  };
}

function composePrompt(prompt: string, testBaseline: string | undefined): string {
  const sections = [prompt];
  if (testBaseline) sections.push(`## Test baseline\n${testBaseline}`);
  sections.push(
    "## Delivery\nWork only inside the current git worktree. When the change is complete, commit it with git so the work is captured as a commit."
  );
  return sections.join("\n\n");
}

async function gitHead(cwd: string, env: NodeJS.ProcessEnv): Promise<string | undefined> {
  try {
    const { stdout } = await execFileAsync("git", ["rev-parse", "HEAD"], { cwd, env });
    return stdout.trim() || undefined;
  } catch {
    return undefined;
  }
}

async function pumpLines(stream: NodeJS.ReadableStream, onLine: (line: string) => void): Promise<void> {
  const rl = readline.createInterface({ input: stream, crlfDelay: Infinity });
  for await (const line of rl) {
    if (line.length > 0) onLine(line);
  }
}

/**
 * Interprets the agent CLI's `--mode json` stdout (shared by Pi and OMP): assistant
 * messages become `stdout` events, tool starts become `tool_call` events, non-JSON
 * lines pass through as raw `stdout`. High-volume bookkeeping events are dropped.
 */
export class AgentOutputSummary {
  private lastAssistantText = "";
  private rawTail: string[] = [];

  consumeStdoutLine(line: string): AdapterEvent[] {
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      this.rawTail.push(line);
      if (this.rawTail.length > 50) this.rawTail.shift();
      return [{ event_type: "stdout", payload: line }];
    }
    if (!isRecord(parsed)) return [{ event_type: "stdout", payload: line }];

    if (parsed.type === "message_end" && isRecord(parsed.message) && parsed.message.role === "assistant") {
      const text = extractText(parsed.message.content);
      if (text.length === 0) return [];
      this.lastAssistantText = text;
      return [{ event_type: "stdout", payload: text }];
    }
    if (parsed.type === "tool_execution_start" && typeof parsed.toolName === "string") {
      const data: Record<string, unknown> = { tool: parsed.toolName };
      if (typeof parsed.toolCallId === "string") data.call_id = parsed.toolCallId;
      if (parsed.args !== undefined && JSON.stringify(parsed.args).length <= MAX_TOOL_ARGS_BYTES) {
        data.args = parsed.args;
      }
      return [{ event_type: "tool_call", payload: parsed.toolName, data }];
    }
    return [];
  }

  output(): string {
    const text = this.lastAssistantText || this.rawTail.join("\n");
    return Buffer.byteLength(text) > MAX_EVENT_BYTES ? text.slice(-MAX_EVENT_BYTES / 4) : text;
  }
}

function extractText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => (isRecord(part) && part.type === "text" && typeof part.text === "string" ? part.text : ""))
    .join("");
}
