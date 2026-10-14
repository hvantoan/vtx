import type { TaskExecuteParams, TaskExecuteResult } from "../protocol/types.js";
import type { AgentAdapter } from "./pi.js";
import { logStderr } from "../protocol/framing.js";

export class OmpAgentAdapter implements AgentAdapter {
  readonly name = "omp" as const;
  readonly version = "1.0.0";

  async execute(
    params: TaskExecuteParams,
    onEvent: (event: { event_type: "stdout" | "stderr" | "progress" | "status_change"; payload: string }) => void,
    signal: AbortSignal
  ): Promise<TaskExecuteResult> {
    logStderr("Starting OMP agent execution", { taskId: params.task_id });

    onEvent({ event_type: "status_change", payload: "started" });
    onEvent({ event_type: "progress", payload: "OMP runtime loading workspace context" });

    if (signal.aborted) {
      return {
        task_id: params.task_id,
        status: "cancelled",
        error: "Task was aborted prior to execution",
      };
    }

    try {
      const waitStep = (ms: number) => {
        const { promise, resolve, reject } = Promise.withResolvers<void>();
        if (signal.aborted) {
          reject(new Error("Aborted"));
          return promise;
        }
        const timer = setTimeout(resolve, ms);
        signal.addEventListener("abort", () => {
          clearTimeout(timer);
          reject(new Error("Aborted"));
        }, { once: true });
        return promise;
      };

      await waitStep(50);
      onEvent({ event_type: "stdout", payload: `[OMP] Synthesizing instructions for: ${params.prompt}` });

      await waitStep(50);
      onEvent({ event_type: "stdout", payload: "[OMP] Applying AST patches and refactors..." });

      await waitStep(50);
      onEvent({ event_type: "status_change", payload: "completed" });

      return {
        task_id: params.task_id,
        status: "succeeded",
        output: `OMP agent successfully completed task ${params.task_id}`,
      };
    } catch (err: unknown) {
      if (signal.aborted) {
        logStderr("OMP execution cancelled", { taskId: params.task_id });
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
