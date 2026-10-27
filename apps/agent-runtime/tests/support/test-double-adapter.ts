import { AgentUnavailableError } from "../../src/adapters/types.js";
import type { AdapterEvent, AdapterRequest, AgentAdapter } from "../../src/adapters/types.js";
import type { AgentType, TaskExecuteResult } from "../../src/protocol/types.js";

export interface TestDoubleOptions {
  /** Events emitted when execute() starts. */
  events?: AdapterEvent[];
  /** When true, execute() blocks until release() is called or the task is aborted. */
  hold?: boolean;
  /** When set, preflight() rejects with AgentUnavailableError carrying this message. */
  preflightError?: string;
  /** When set, execute() throws this message. */
  throwOnExecute?: string;
}

/**
 * TEST DOUBLE — simulation adapter used ONLY by unit tests to exercise the IPC protocol.
 * It never spawns a real agent, always reports `isSimulation = true` (surfaced on the wire as
 * `test_double: true`), and is intentionally located under tests/ so it can never be bundled
 * into the production host path. Deterministic: no timers, completion is driven by promises.
 */
export class TestDoubleAdapter implements AgentAdapter {
  readonly version = "test-double-1.0.0";
  readonly isSimulation = true;
  readonly requests: AdapterRequest[] = [];
  /** Resolves once execute() has emitted its scripted events. */
  readonly started: Promise<void>;
  private readonly startedSignal = Promise.withResolvers<void>();
  private readonly releaseSignal = Promise.withResolvers<void>();

  constructor(
    readonly name: AgentType,
    private readonly options: TestDoubleOptions = {}
  ) {
    this.started = this.startedSignal.promise;
  }

  release(): void {
    this.releaseSignal.resolve();
  }

  async preflight(): Promise<void> {
    if (this.options.preflightError) throw new AgentUnavailableError(this.options.preflightError);
  }

  async execute(request: AdapterRequest): Promise<TaskExecuteResult> {
    this.requests.push(request);
    const { params, onEvent, signal } = request;
    if (this.options.throwOnExecute) throw new Error(this.options.throwOnExecute);

    const events = this.options.events ?? [
      { event_type: "status_change", payload: "started" },
      { event_type: "progress", payload: "test double working" },
      { event_type: "stdout", payload: `[TEST DOUBLE] ${params.prompt}` },
    ];
    for (const event of events) onEvent(event);
    this.startedSignal.resolve();

    if (this.options.hold) {
      const aborted = Promise.withResolvers<void>();
      signal.addEventListener("abort", () => aborted.resolve(), { once: true });
      if (signal.aborted) aborted.resolve();
      await Promise.race([this.releaseSignal.promise, aborted.promise]);
    }
    if (signal.aborted) {
      const reason = typeof signal.reason === "string" ? signal.reason : "operator_cancel";
      return { task_id: params.task_id, status: "cancelled", error: `cancelled: ${reason}` };
    }
    return { task_id: params.task_id, status: "succeeded", output: "test double finished" };
  }
}
