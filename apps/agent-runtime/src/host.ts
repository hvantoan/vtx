import { realpath, stat } from "node:fs/promises";
import * as path from "node:path";
import { pathToFileURL } from "node:url";
import { OmpCliAdapter } from "./adapters/omp.js";
import { PiCliAdapter } from "./adapters/pi.js";
import { AgentUnavailableError } from "./adapters/types.js";
import type { AdapterEvent, AgentAdapter } from "./adapters/types.js";
import { StdioChannel, logStderr } from "./protocol/framing.js";
import {
  classifyIncoming,
  parseInitializeParams,
  parseTaskCancelParams,
  parseTaskExecuteParams,
} from "./protocol/guards.js";
import { DEFAULT_HEARTBEAT_MS, ErrorCode, MAX_EVENT_BYTES, PROTOCOL_VERSION } from "./protocol/types.js";
import type {
  AgentType,
  HostHeartbeatParams,
  HostLogParams,
  InitializeResult,
  JsonRpcError,
  JsonRpcRequest,
  RequestId,
  TaskEventParams,
  TaskExecuteParams,
  TaskExecuteResult,
} from "./protocol/types.js";
import { FileSessionRegistry } from "./sessions/registry.js";
import type { AgentSessionRecord, SessionRegistry } from "./sessions/registry.js";

const BASE_ENV_NAMES = ["PATH", "HOME", "TMPDIR", "LANG"];
const ENV_NAME_PATTERN = /^[A-Za-z_][A-Za-z0-9_]*$/;
const SECRET_NAME_PATTERN = /KEY|TOKEN|SECRET|PASSWORD|CREDENTIAL|AUTH/i;
const MIN_SECRET_LENGTH = 8;
const MAX_LOG_MESSAGE_BYTES = 4096;
const TRUNCATION_MARKER = "…[truncated]";

export interface AgentHostOptions {
  /** Adapters available to `initialize.agent_type`. Defaults to the real Pi and OMP CLI adapters. */
  adapters?: AgentAdapter[];
  /** Optional durable session/event log shaped like `agent_sessions` / `agent_events`. */
  registry?: SessionRegistry;
  /** Called after the `exit` notification (or stdin EOF). Defaults to `process.exit`. */
  onExit?: (code: number) => void;
  /** Environment the allowlisted child env is picked from. Defaults to the host's own environment. */
  baseEnv?: NodeJS.ProcessEnv;
}

interface InitializedState {
  agentType: AgentType;
  adapter: AgentAdapter;
  protocolVersion: string;
  heartbeatMs: number | undefined;
  runId: string | undefined;
  envNames: string[];
}

interface ActiveTask {
  taskId: string;
  controller: AbortController;
  seq: number;
  done: Promise<void>;
  finish: () => void;
}

export class AgentHost {
  private readonly channel: StdioChannel;
  private readonly adapters = new Map<AgentType, AgentAdapter>();
  private readonly registry: SessionRegistry | undefined;
  private readonly onExit: (code: number) => void;
  private readonly baseEnv: NodeJS.ProcessEnv;
  private initialized: InitializedState | undefined;
  private active: ActiveTask | undefined;
  private shuttingDown = false;
  private exiting = false;

  constructor(channel: StdioChannel = new StdioChannel(), options: AgentHostOptions = {}) {
    this.channel = channel;
    this.registry = options.registry;
    this.onExit = options.onExit ?? ((code) => process.exit(code));
    this.baseEnv = options.baseEnv ?? hostEnvironment();
    for (const adapter of options.adapters ?? [new PiCliAdapter(), new OmpCliAdapter()]) {
      this.registerAdapter(adapter);
    }
    this.channel.onLine((line) => {
      void this.handleLine(line);
    });
    this.channel.onClose(() => {
      void this.handleInputClosed();
    });
  }

  registerAdapter(adapter: AgentAdapter): void {
    this.adapters.set(adapter.name, adapter);
  }

  /** Diagnostic mirror of stderr lines as a `host/log` notification. */
  log(level: HostLogParams["level"], message: string, meta?: Record<string, unknown>): void {
    const params: HostLogParams = { level, message: truncateUtf8(message, MAX_LOG_MESSAGE_BYTES), meta };
    this.channel.sendNotification({ jsonrpc: "2.0", method: "host/log", params });
  }

  private respondError(id: RequestId | null, code: number, message: string, data?: unknown): void {
    const error: JsonRpcError = { code, message, data };
    this.channel.sendResponse({ jsonrpc: "2.0", id, error });
  }

  private async handleLine(line: string): Promise<void> {
    let value: unknown;
    try {
      value = JSON.parse(line);
    } catch (err: unknown) {
      logStderr("Failed to parse incoming line as JSON-RPC", { error: String(err) });
      this.respondError(null, ErrorCode.ParseError, "Parse error");
      this.log("warn", "skipped malformed inbound line", { bytes: Buffer.byteLength(line) });
      return;
    }

    const incoming = classifyIncoming(value);
    if (incoming.kind === "invalid") {
      this.respondError(incoming.id, ErrorCode.InvalidRequest, incoming.reason);
      return;
    }
    if (incoming.kind === "notification") {
      this.handleNotification(incoming.message.method, incoming.message.params);
      return;
    }

    const req = incoming.message;
    try {
      switch (req.method) {
        case "initialize":
          await this.handleInitialize(req);
          break;
        case "task/execute":
          await this.handleTaskExecute(req);
          break;
        case "task/cancel":
          this.handleTaskCancelRequest(req);
          break;
        case "shutdown":
          await this.handleShutdown(req);
          break;
        default:
          this.respondError(req.id, ErrorCode.MethodNotFound, `Method not found: ${req.method}`);
      }
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err);
      logStderr("Unhandled error during method execution", { method: req.method, error: message });
      this.respondError(req.id, ErrorCode.InternalError, `Internal error: ${message}`);
    }
  }

  private handleNotification(method: string, params: unknown): void {
    switch (method) {
      case "task/cancel": {
        const cancel = parseTaskCancelParams(params);
        if (cancel) this.cancelTask(cancel.task_id, cancel.reason ?? "operator_cancel");
        return;
      }
      case "exit":
        this.terminate();
        return;
      default:
        // JSON-RPC: notifications never receive an error response.
        this.log("warn", `ignored unsupported notification: ${method}`);
    }
  }

  private async handleInitialize(req: JsonRpcRequest): Promise<void> {
    if (this.initialized) {
      this.respondError(req.id, ErrorCode.InvalidRequest, "Already initialized");
      return;
    }
    const parsed = parseInitializeParams(req.params);
    if (!parsed.ok) {
      this.respondError(req.id, ErrorCode.InvalidParams, `Invalid params: ${parsed.message}`);
      return;
    }
    const params = parsed.value;

    const [hostMajor, hostMinor] = parseVersion(PROTOCOL_VERSION);
    const [reqMajor, reqMinor] = parseVersion(params.protocol_version);
    if (reqMajor !== hostMajor) {
      this.respondError(req.id, ErrorCode.UnsupportedProtocolVersion, `Unsupported protocol version ${params.protocol_version}`, {
        supported: PROTOCOL_VERSION,
      });
      return;
    }
    const negotiatedMinor = Math.min(reqMinor, hostMinor);
    const negotiatedVersion = `${hostMajor}.${negotiatedMinor}`;

    const agentType: AgentType = params.agent_type ?? "pi";
    const adapter = this.adapters.get(agentType);
    if (!adapter) {
      this.respondError(req.id, ErrorCode.AgentUnavailable, `No adapter registered for ${agentType}`);
      return;
    }

    const envNames = mergeEnvNames(params.env_allowlist);
    try {
      await adapter.preflight(this.pickEnv(envNames));
    } catch (err: unknown) {
      const code = err instanceof AgentUnavailableError ? ErrorCode.AgentUnavailable : ErrorCode.InternalError;
      this.respondError(req.id, code, err instanceof Error ? err.message : String(err));
      return;
    }

    // host/heartbeat is a v1.1 feature; a v1.0 peer never expects it.
    const heartbeatMs = negotiatedMinor >= 1 ? (params.capabilities?.heartbeat_ms ?? DEFAULT_HEARTBEAT_MS) : undefined;
    this.initialized = { agentType, adapter, protocolVersion: negotiatedVersion, heartbeatMs, runId: params.run_id, envNames };

    const result: InitializeResult = {
      protocol_version: negotiatedVersion,
      agent_type: agentType,
      agent_version: adapter.version,
      host: { node_version: process.version, runtime_pid: process.pid },
      adapters: [...this.adapters.values()].map((a) => ({ name: a.name, version: a.version, test_double: a.isSimulation })),
      capabilities: { streaming: true, cancellation: true, heartbeat_ms: heartbeatMs, max_event_bytes: MAX_EVENT_BYTES },
    };
    logStderr("Initialized agent runtime", { agentType, protocolVersion: negotiatedVersion });
    this.channel.sendResponse({ jsonrpc: "2.0", id: req.id, result });
  }

  private async handleTaskExecute(req: JsonRpcRequest): Promise<void> {
    const state = this.initialized;
    if (!state) {
      this.respondError(req.id, ErrorCode.InvalidRequest, "initialize must be the first message");
      return;
    }
    if (this.shuttingDown) {
      this.respondError(req.id, ErrorCode.InvalidRequest, "Host is shutting down");
      return;
    }
    const parsed = parseTaskExecuteParams(req.params);
    if (!parsed.ok) {
      this.respondError(req.id, ErrorCode.InvalidParams, `Invalid params: ${parsed.message}`);
      return;
    }
    const params = parsed.value;
    if (this.active) {
      if (this.active.taskId === params.task_id) {
        this.respondError(req.id, ErrorCode.InvalidRequest, `Task ${params.task_id} is already actively running`);
      } else {
        this.respondError(req.id, ErrorCode.HostBusy, `Host is busy with task ${this.active.taskId}`);
      }
      return;
    }

    // Reserve the single task slot synchronously so concurrent requests cannot interleave.
    const { promise: done, resolve: finish } = Promise.withResolvers<void>();
    const task: ActiveTask = { taskId: params.task_id, controller: new AbortController(), seq: 0, done, finish: () => finish() };
    this.active = task;

    try {
      const workspacePath = await resolveWorkspace(params.workspace_path);
      if (workspacePath === undefined) {
        this.respondError(req.id, ErrorCode.WorkspaceInvalid, `workspace_path is not an existing directory: ${params.workspace_path}`);
        return;
      }
      const result = await this.runTask(state, task, params, workspacePath);
      this.channel.sendResponse({ jsonrpc: "2.0", id: req.id, result });
    } finally {
      this.active = undefined;
      task.finish();
    }
  }

  private async runTask(
    state: InitializedState,
    task: ActiveTask,
    params: TaskExecuteParams,
    workspacePath: string
  ): Promise<TaskExecuteResult> {
    const { adapter } = state;
    const startedAt = Date.now();
    const env = this.pickEnv(mergeEnvNames(state.envNames, params.env_allowlist));
    const redact = buildRedactor(env);
    const session = await this.beginSession(state, params, workspacePath);
    let persistChain: Promise<void> = Promise.resolve();

    const emit = (event: AdapterEvent): void => {
      task.seq += 1;
      const payload = truncateUtf8(redact(event.payload ?? ""), MAX_EVENT_BYTES);
      const data = event.data ? redactData(event.data, redact) : undefined;
      const notification: TaskEventParams = {
        task_id: params.task_id,
        seq: task.seq,
        event_type: event.event_type,
        payload,
        data,
        timestamp: new Date().toISOString(),
      };
      this.channel.sendNotification({ jsonrpc: "2.0", method: "task/event", params: notification });
      if (session) {
        const record = {
          session_id: session.session_id,
          run_id: session.run_id,
          seq: notification.seq,
          event_type: notification.event_type,
          payload,
          data: data ?? null,
          event_ts: notification.timestamp,
        };
        persistChain = persistChain.then(() => this.persist(() => this.registry?.appendEvent(record)));
      }
    };

    const timer =
      params.timeout_ms === undefined
        ? undefined
        : setTimeout(() => {
            logStderr("Task timeout reached", { taskId: params.task_id, timeoutMs: params.timeout_ms });
            task.controller.abort("timeout");
          }, params.timeout_ms);
    const heartbeat =
      state.heartbeatMs === undefined
        ? undefined
        : setInterval(() => {
            const beat: HostHeartbeatParams = {
              task_id: params.task_id,
              pid: process.pid,
              last_event_seq: task.seq,
              timestamp: new Date().toISOString(),
            };
            this.channel.sendNotification({ jsonrpc: "2.0", method: "host/heartbeat", params: beat });
          }, state.heartbeatMs);

    let result: TaskExecuteResult;
    try {
      result = await adapter.execute({ params, workspacePath, env, onEvent: emit, signal: task.controller.signal });
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err);
      logStderr("Adapter threw during execute", { taskId: params.task_id, error: message });
      result = task.controller.signal.aborted
        ? { task_id: params.task_id, status: "cancelled", error: `cancelled: ${abortReason(task.controller.signal)}` }
        : { task_id: params.task_id, status: "failed", error: message };
    } finally {
      clearTimeout(timer);
      clearInterval(heartbeat);
    }

    const finalResult: TaskExecuteResult = {
      ...result,
      task_id: params.task_id,
      test_double: adapter.isSimulation,
      error: result.error === undefined ? undefined : redact(result.error),
      output: result.output === undefined ? undefined : redact(truncateUtf8(result.output, MAX_EVENT_BYTES)),
      usage: { duration_ms: Date.now() - startedAt, ...result.usage },
    };
    if (finalResult.status === "cancelled" && finalResult.error === undefined) {
      finalResult.error = `cancelled: ${abortReason(task.controller.signal)}`;
    }

    await persistChain;
    if (session) {
      await this.persist(() =>
        this.registry?.updateSession(session.session_id, {
          status: finalResult.status,
          exit_reason: finalResult.status === "cancelled" ? abortReason(task.controller.signal) : null,
          error_message: finalResult.status === "succeeded" ? null : (finalResult.error ?? null),
          finished_at: new Date().toISOString(),
        })
      );
    }
    return finalResult;
  }

  private async beginSession(
    state: InitializedState,
    params: TaskExecuteParams,
    workspacePath: string
  ): Promise<AgentSessionRecord | undefined> {
    const registry = this.registry;
    const runId = state.runId;
    if (!registry || !runId) return undefined;
    let record: AgentSessionRecord | undefined;
    await this.persist(async () => {
      record = await registry.beginSession({
        run_id: runId,
        attempt: params.attempt ?? 1,
        agent_type: state.agentType,
        agent_version: state.adapter.version,
        protocol_version: state.protocolVersion,
        is_test_double: state.adapter.isSimulation,
        status: "running",
        worktree_path: workspacePath,
        pid: process.pid,
        exit_reason: null,
        error_code: null,
        error_message: null,
        started_at: new Date().toISOString(),
        finished_at: null,
      });
    });
    return record;
  }

  /** Persistence is evidence, not control flow: failures are surfaced as host/log and never fail the task. */
  private async persist(op: () => Promise<unknown> | undefined): Promise<void> {
    try {
      await op();
    } catch (err: unknown) {
      this.log("error", "session registry write failed", { error: err instanceof Error ? err.message : String(err) });
    }
  }

  private cancelTask(taskId: string, reason: string): boolean {
    if (!this.active || this.active.taskId !== taskId) return false;
    logStderr("Cancelling active task", { taskId, reason });
    this.active.controller.abort(reason);
    return true;
  }

  private handleTaskCancelRequest(req: JsonRpcRequest): void {
    const cancel = parseTaskCancelParams(req.params);
    if (!cancel) {
      this.respondError(req.id, ErrorCode.InvalidParams, "Invalid params: task_id is required");
      return;
    }
    if (this.cancelTask(cancel.task_id, cancel.reason ?? "operator_cancel")) {
      this.channel.sendResponse({ jsonrpc: "2.0", id: req.id, result: { cancelled: true, task_id: cancel.task_id } });
    } else {
      this.respondError(req.id, ErrorCode.TaskNotFound, `Task ${cancel.task_id} is not running`);
    }
  }

  /** Cancels the active task and waits for its terminal result to be written before acknowledging. */
  private async handleShutdown(req: JsonRpcRequest): Promise<void> {
    logStderr("Shutdown requested by supervisor");
    this.shuttingDown = true;
    const active = this.active;
    if (active) {
      active.controller.abort("shutdown");
      await active.done;
    }
    this.channel.sendResponse({ jsonrpc: "2.0", id: req.id, result: {} });
  }

  private async handleInputClosed(): Promise<void> {
    if (this.exiting) return;
    this.shuttingDown = true;
    const active = this.active;
    if (active) {
      active.controller.abort("shutdown");
      await active.done;
    }
    this.terminate();
  }

  private terminate(): void {
    if (this.exiting) return;
    this.exiting = true;
    this.shuttingDown = true;
    this.active?.controller.abort("shutdown");
    this.channel.close();
    this.onExit(0);
  }

  private pickEnv(names: string[]): NodeJS.ProcessEnv {
    const env: NodeJS.ProcessEnv = {};
    for (const name of names) {
      const value = this.baseEnv[name];
      if (value !== undefined) env[name] = value;
    }
    return env;
  }
}

function hostEnvironment(): NodeJS.ProcessEnv {
  return process["env"];
}

function parseVersion(version: string): [number, number] {
  const [major, minor] = version.split(".").map(Number);
  return [major ?? 0, minor ?? 0];
}

function mergeEnvNames(...lists: Array<string[] | undefined>): string[] {
  const names = new Set(BASE_ENV_NAMES);
  for (const list of lists) {
    for (const name of list ?? []) {
      if (ENV_NAME_PATTERN.test(name)) names.add(name);
    }
  }
  return [...names];
}

async function resolveWorkspace(workspacePath: string): Promise<string | undefined> {
  if (!path.isAbsolute(workspacePath)) return undefined;
  try {
    const canonical = await realpath(workspacePath);
    return (await stat(canonical)).isDirectory() ? canonical : undefined;
  } catch {
    return undefined;
  }
}

function abortReason(signal: AbortSignal): string {
  return typeof signal.reason === "string" ? signal.reason : "operator_cancel";
}

function truncateUtf8(text: string, maxBytes: number): string {
  if (Buffer.byteLength(text) <= maxBytes) return text;
  const budget = maxBytes - Buffer.byteLength(TRUNCATION_MARKER);
  const head = Buffer.from(text).subarray(0, budget).toString("utf8").replace(/\uFFFD+$/, "");
  return head + TRUNCATION_MARKER;
}

/** Masks values of secret-looking env vars passed to the agent so they never reach events, results, or logs. */
function buildRedactor(env: NodeJS.ProcessEnv): (text: string) => string {
  const secrets: string[] = [];
  for (const [name, value] of Object.entries(env)) {
    if (value !== undefined && value.length >= MIN_SECRET_LENGTH && SECRET_NAME_PATTERN.test(name)) {
      secrets.push(value);
    }
  }
  return (text) => secrets.reduce((acc, secret) => acc.split(secret).join("[REDACTED]"), text);
}

function redactData(data: Record<string, unknown>, redact: (text: string) => string): Record<string, unknown> {
  const parsed: unknown = JSON.parse(redact(JSON.stringify(data)));
  return typeof parsed === "object" && parsed !== null && !Array.isArray(parsed) ? Object.fromEntries(Object.entries(parsed)) : {};
}

/** Routes console output to `host/log` so stray writes can never corrupt the NDJSON stdout stream. */
function installConsoleBridge(host: AgentHost): void {
  const forward =
    (level: HostLogParams["level"]) =>
    (...args: unknown[]): void => {
      host.log(level, args.map(String).join(" "));
    };
  console.log = forward("info");
  console.info = forward("info");
  console.debug = forward("debug");
  console.warn = forward("warn");
  console.error = forward("error");
}

// Entrypoint when executed directly
if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  logStderr("VTX Agent Runtime Host starting on stdio...");
  const registryDir = hostEnvironment().VTX_SESSION_REGISTRY_DIR;
  const host = new AgentHost(new StdioChannel(), {
    registry: registryDir ? new FileSessionRegistry(registryDir) : undefined,
  });
  installConsoleBridge(host);
}
