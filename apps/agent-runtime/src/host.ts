import { StdioChannel, logStderr } from "./protocol/framing.js";
import type {
  InitializeParams,
  InitializeResult,
  JsonRpcRequest,
  TaskCancelParams,
  TaskEventParams,
  TaskExecuteParams,
} from "./protocol/types.js";
import type { AgentAdapter } from "./adapters/pi.js";
import { PiAgentAdapter } from "./adapters/pi.js";
import { OmpAgentAdapter } from "./adapters/omp.js";

export class AgentHost {
  private channel: StdioChannel;
  private adapters = new Map<string, AgentAdapter>();
  private activeTasks = new Map<string, AbortController>();
  private currentAgentType: "pi" | "omp" = "pi";
  private eventSeq = 0;

  constructor(channel: StdioChannel = new StdioChannel()) {
    this.channel = channel;
    this.registerAdapter(new PiAgentAdapter());
    this.registerAdapter(new OmpAgentAdapter());

    this.channel.onLine((line) => {
      this.handleLine(line);
    });
  }

  registerAdapter(adapter: AgentAdapter): void {
    this.adapters.set(adapter.name, adapter);
  }

  private async handleLine(line: string): Promise<void> {
    let req: JsonRpcRequest;
    try {
      req = JSON.parse(line) as JsonRpcRequest;
    } catch (err: unknown) {
      logStderr("Failed to parse incoming line as JSON-RPC", { line });
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: null,
        error: { code: -32700, message: "Parse error" },
      });
      return;
    }

    if (req.jsonrpc !== "2.0") {
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id ?? null,
        error: { code: -32600, message: "Invalid Request: jsonrpc version must be 2.0" },
      });
      return;
    }

    try {
      switch (req.method) {
        case "initialize":
          this.handleInitialize(req);
          break;
        case "task/execute":
          await this.handleTaskExecute(req);
          break;
        case "task/cancel":
          this.handleTaskCancel(req);
          break;
        case "shutdown":
          this.handleShutdown(req);
          break;
        default:
          this.channel.sendResponse({
            jsonrpc: "2.0",
            id: req.id,
            error: { code: -32601, message: `Method not found: ${req.method}` },
          });
      }
    } catch (err: unknown) {
      const message = err instanceof Error ? err.message : String(err);
      logStderr("Unhandled error during method execution", { method: req.method, error: message });
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        error: { code: -32603, message: `Internal error: ${message}` },
      });
    }
  }

  private handleInitialize(req: JsonRpcRequest): void {
    const params = (req.params ?? {}) as InitializeParams;
    if (params.agent_type === "omp") {
      this.currentAgentType = "omp";
    } else {
      this.currentAgentType = "pi";
    }

    const adapter = this.adapters.get(this.currentAgentType);
    const result: InitializeResult = {
      protocol_version: "1.0",
      agent_type: this.currentAgentType,
      agent_version: adapter?.version ?? "1.0.0",
      capabilities: {
        streaming: true,
        cancellation: true,
      },
    };

    logStderr("Initialized agent runtime", { agentType: this.currentAgentType });
    this.channel.sendResponse({
      jsonrpc: "2.0",
      id: req.id,
      result,
    });
  }

  private async handleTaskExecute(req: JsonRpcRequest): Promise<void> {
    const params = req.params as TaskExecuteParams;
    if (!params?.task_id || !params?.prompt) {
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        error: { code: -32602, message: "Invalid params: task_id and prompt are required" },
      });
      return;
    }

    const adapter = this.adapters.get(this.currentAgentType);
    if (!adapter) {
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        error: { code: -32603, message: `No adapter registered for ${this.currentAgentType}` },
      });
      return;
    }
    if (this.activeTasks.has(params.task_id)) {
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        error: { code: -32600, message: `Task ${params.task_id} is already actively running` },
      });
      return;
    }


    const abortController = new AbortController();
    this.activeTasks.set(params.task_id, abortController);

    // Timeout handling
    let timer: NodeJS.Timeout | undefined;
    if (params.timeout_ms && params.timeout_ms > 0) {
      timer = setTimeout(() => {
        logStderr("Task timeout reached", { taskId: params.task_id, timeoutMs: params.timeout_ms });
        abortController.abort();
      }, params.timeout_ms);
    }

    try {
      const result = await adapter.execute(
        params,
        (event) => {
          this.eventSeq += 1;
          const notificationPayload: TaskEventParams = {
            task_id: params.task_id,
            seq: this.eventSeq,
            event_type: event.event_type,
            payload: event.payload,
            timestamp: new Date().toISOString(),
          };

          this.channel.sendNotification({
            jsonrpc: "2.0",
            method: "task/event",
            params: notificationPayload,
          });
        },
        abortController.signal
      );

      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        result,
      });
    } finally {
      clearTimeout(timer);
      this.activeTasks.delete(params.task_id);
    }
  }

  private handleTaskCancel(req: JsonRpcRequest): void {
    const params = req.params as TaskCancelParams;
    if (params?.task_id && this.activeTasks.has(params.task_id)) {
      logStderr("Cancelling active task", { taskId: params.task_id, reason: params.reason });
      const controller = this.activeTasks.get(params.task_id);
      controller?.abort();
    }
    // If it's a request with an id, send response
    if (req.id !== undefined && req.id !== null) {
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        result: { cancelled: true, task_id: params?.task_id },
      });
    }
  }

  private handleShutdown(req: JsonRpcRequest): void {
    logStderr("Shutdown requested by supervisor");
    // Cancel all running tasks
    for (const controller of this.activeTasks.values()) {
      controller.abort();
    }
    this.activeTasks.clear();

    if (req.id !== undefined && req.id !== null) {
      this.channel.sendResponse({
        jsonrpc: "2.0",
        id: req.id,
        result: { ok: true },
      });
    }

    setTimeout(() => {
      this.channel.close();
      process.exit(0);
    }, 50);
  }
}

// Entrypoint when executed directly
if (import.meta.url === `file://${process.argv[1]}`) {
  logStderr("VTX Agent Runtime Host starting on stdio...");
  new AgentHost();
}
