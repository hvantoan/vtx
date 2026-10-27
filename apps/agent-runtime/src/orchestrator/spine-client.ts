import net from "node:net";
import { randomUUID } from "node:crypto";
import { SpineError, isSpineResponse } from "./protocol.js";
import type { SpineRequest, SpineResponse } from "./protocol.js";

export interface SpineClientOptions {
  /** Absolute path to the spine unix-domain socket (SPINE_SOCKET_PATH). */
  socketPath: string;
  /** Shared secret presented on every request (SPINE_AUTH_TOKEN). */
  token: string;
  /** Per-request timeout. A timeout fails closed as a service error. */
  requestTimeoutMs?: number;
  /**
   * Per-tool overrides for {@link requestTimeoutMs}. A synchronous tool that
   * does real work (a verification suite) needs minutes, not the fast RPC
   * default; timing it out would abandon work the spine still completes.
   */
  toolTimeouts?: Readonly<Record<string, number>>;
  /** Connection timeout for {@link SpineClient.connect}. */
  connectTimeoutMs?: number;
}

interface Pending {
  resolve: (value: unknown) => void;
  reject: (err: SpineError) => void;
  timer: NodeJS.Timeout;
  tool: string;
}

const DEFAULT_REQUEST_TIMEOUT_MS = 30_000;
const DEFAULT_CONNECT_TIMEOUT_MS = 5_000;

/**
 * Newline-delimited JSON client for the Go spine over a unix socket.
 *
 * Fail-closed by construction: if the socket is not connected, drops, or a call
 * times out, every pending and future call rejects with a {@link SpineError}
 * rather than resolving to a fabricated success. The orchestrator treats any
 * such error as "spine unreachable" and blocks the tool.
 */
export class SpineClient {
  private readonly socketPath: string;
  private readonly token: string;
  private readonly requestTimeoutMs: number;
  private readonly toolTimeouts: Readonly<Record<string, number>>;
  private readonly connectTimeoutMs: number;

  private socket: net.Socket | undefined;
  private buffer = "";
  private readonly pending = new Map<string, Pending>();
  private connecting: Promise<void> | undefined;
  private closed = false;

  constructor(options: SpineClientOptions) {
    this.socketPath = options.socketPath;
    this.token = options.token;
    this.requestTimeoutMs = options.requestTimeoutMs ?? DEFAULT_REQUEST_TIMEOUT_MS;
    this.toolTimeouts = options.toolTimeouts ?? {};
    this.connectTimeoutMs = options.connectTimeoutMs ?? DEFAULT_CONNECT_TIMEOUT_MS;
  }

  get isConnected(): boolean {
    return this.socket !== undefined && !this.socket.destroyed && !this.closed;
  }

  /**
   * Open the socket. Rejects if the spine is unreachable within the timeout.
   * Safe to call again after a drop: a heartbeat can re-establish the
   * connection instead of the session dying on a transient disconnect.
   */
  connect(): Promise<void> {
    if (this.closed) {
      return Promise.reject(new SpineError("service_error", "client is closed", "connect"));
    }
    if (this.isConnected) return Promise.resolve();
    if (this.connecting) return this.connecting;

    const attempt = new Promise<void>((resolve, reject) => {
      const socket = net.createConnection(this.socketPath);
      let settled = false;
      const timer = setTimeout(() => {
        if (settled) return;
        settled = true;
        socket.destroy();
        reject(new SpineError("service_error", "spine connect timed out", "connect"));
      }, this.connectTimeoutMs);
      timer.unref?.();

      socket.once("error", (err) => {
        if (settled) {
          this.failAll(new SpineError("service_error", `spine socket error: ${err.message}`, "connect"));
          return;
        }
        settled = true;
        clearTimeout(timer);
        socket.destroy();
        reject(new SpineError("service_error", `spine connect failed: ${err.message}`, "connect"));
      });

      socket.once("connect", () => {
        if (settled) return;
        settled = true;
        clearTimeout(timer);
        this.socket = socket;
        socket.setEncoding("utf8");
        socket.on("data", (chunk) => this.onData(chunk.toString()));
        socket.on("close", () => {
          this.socket = undefined;
          this.failAll(new SpineError("service_error", "spine socket closed", "connect"));
        });
        resolve();
      });
    });
    this.connecting = attempt;
    void attempt.finally(() => {
      if (this.connecting === attempt) this.connecting = undefined;
    }).catch(() => {});
    return attempt;
  }

  /**
   * Send one tool request and await its correlated response. `runId` and
   * `leaseEpoch` are top-level so the spine fences the call without trusting
   * nested args. They are optional only for the unscoped `intake` tool, which
   * creates the intent a run is later born from. Resolves with the spine's
   * `result`; rejects with {@link SpineError} on any non-ok response, timeout,
   * or transport failure.
   */
  call(tool: string, params: { runId?: string; leaseEpoch?: number; args?: unknown }): Promise<unknown> {
    if (!this.isConnected) {
      return Promise.reject(new SpineError("service_error", "spine not connected", tool));
    }
    const id = randomUUID();
    const request: SpineRequest = {
      id,
      token: this.token,
      tool,
      run_id: params.runId ?? "",
      lease_epoch: params.leaseEpoch ?? 0,
      ...(params.args === undefined ? {} : { args: params.args }),
    };

    return new Promise<unknown>((resolve, reject) => {
      const timeoutMs = this.toolTimeouts[tool] ?? this.requestTimeoutMs;
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new SpineError("service_error", `spine call ${tool} timed out`, tool));
      }, timeoutMs);
      timer.unref?.();
      this.pending.set(id, { resolve, reject, timer, tool });

      this.socket?.write(`${JSON.stringify(request)}\n`, (err) => {
        if (err) {
          const p = this.pending.get(id);
          if (p) {
            clearTimeout(p.timer);
            this.pending.delete(id);
          }
          reject(new SpineError("service_error", `spine write failed: ${err.message}`, tool));
        }
      });
    });
  }

  /** Idempotent teardown. Safe to call from session_shutdown more than once. */
  close(): void {
    if (this.closed) return;
    this.closed = true;
    this.failAll(new SpineError("service_error", "spine client closed", "close"));
    this.socket?.destroy();
    this.socket = undefined;
  }

  private onData(chunk: string): void {
    this.buffer += chunk;
    let newline = this.buffer.indexOf("\n");
    while (newline !== -1) {
      const line = this.buffer.slice(0, newline).trim();
      this.buffer = this.buffer.slice(newline + 1);
      newline = this.buffer.indexOf("\n");
      if (line.length === 0) continue;
      this.handleLine(line);
    }
  }

  private handleLine(line: string): void {
    let parsed: unknown;
    try {
      parsed = JSON.parse(line);
    } catch {
      return; // Ignore malformed frames; the request timeout fails the call closed.
    }
    if (!isSpineResponse(parsed)) return;
    this.deliver(parsed);
  }

  private deliver(response: SpineResponse): void {
    const pending = this.pending.get(response.id);
    if (!pending) return;
    clearTimeout(pending.timer);
    this.pending.delete(response.id);
    if (response.ok) {
      pending.resolve(response.result);
      return;
    }
    const code = response.error?.code ?? "service_error";
    const message = response.error?.message ?? "unknown spine error";
    pending.reject(new SpineError(code, message, response.tool || pending.tool));
  }

  private failAll(err: SpineError): void {
    for (const [, pending] of this.pending) {
      clearTimeout(pending.timer);
      pending.reject(err);
    }
    this.pending.clear();
  }
}
