import type { TaskExecuteParams, TaskExecuteResult } from "../protocol/types.js";
import { logStderr } from "../protocol/framing.js";

export interface AgentAdapter {
  readonly name: "pi" | "omp";
  readonly version: string;
  readonly isSimulation: boolean;
  execute(
    params: TaskExecuteParams,
    onEvent: (event: { event_type: "stdout" | "stderr" | "progress" | "status_change"; payload: string }) => void,
    signal: AbortSignal
  ): Promise<TaskExecuteResult>;
}

/**
 * PiAgentAdapter (Simulation Double for Bootstrap phase)
 *
 * NOTE: In Phase 0 Bootstrap (VTX-14), this adapter operates as an IPC protocol double
 * to verify NDJSON framing, bidirectional event streaming, cancellation (AbortSignal),
 * backpressure, and process isolation.
 * Real agent execution (Pi CLI/SDK binary orchestration) is formally deferred to VTX-7.
 */
export class PiAgentAdapter implements AgentAdapter {
  readonly name = "pi" as const;
  readonly version = "1.0.0";
  readonly isSimulation = true;
  async execute(
    params: TaskExecuteParams,
    onEvent: (event: { event_type: "stdout" | "stderr" | "progress" | "status_change"; payload: string }) => void,
    signal: AbortSignal
  ): Promise<TaskExecuteResult> {
    logStderr("Starting Pi agent execution", { taskId: params.task_id });

    onEvent({ event_type: "status_change", payload: "started" });
    onEvent({ event_type: "progress", payload: "Pi agent analyzing prompt" });

    // Check cancellation
    if (signal.aborted) {
      return {
        task_id: params.task_id,
        status: "cancelled",
        error: "Task was aborted prior to execution",
      };
    }

    // Simulate agent steps with cancellation check
    try {
      await sleep(50, signal);
      onEvent({ event_type: "stdout", payload: `[Pi] Planning solution for: ${params.prompt}` });

      await sleep(50, signal);
      onEvent({ event_type: "stdout", payload: "[Pi] Executing code modifications..." });

      await sleep(50, signal);
      onEvent({ event_type: "status_change", payload: "completed" });

      return {
        task_id: params.task_id,
        status: "succeeded",
        output: `Pi agent successfully executed task ${params.task_id}`,
      };
    } catch (err: unknown) {
      if (signal.aborted) {
        logStderr("Pi execution cancelled", { taskId: params.task_id });
        return {
          task_id: params.task_id,
          status: "cancelled",
          error: "Execution was cancelled by supervisor",
        };
      }
      const message = err instanceof Error ? err.message : String(err);
      return {
        task_id: params.task_id,
        status: "failed",
        error: message,
      };
    }
  }
}

function sleep(ms: number, signal?: AbortSignal): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  if (signal?.aborted) {
    reject(new Error("Aborted"));
    return promise;
  }
  const timer = setTimeout(resolve, ms);
  signal?.addEventListener("abort", () => {
    clearTimeout(timer);
    reject(new Error("Aborted"));
  }, { once: true });
  return promise;
}
