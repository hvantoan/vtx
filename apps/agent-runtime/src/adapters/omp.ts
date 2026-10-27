import { CliAgentAdapter } from "./cli.js";
import type { CliAdapterOptions } from "./cli.js";

/** Secondary real adapter: shells out to the `omp` CLI in non-interactive JSON mode. */
export class OmpCliAdapter extends CliAgentAdapter {
  readonly name = "omp" as const;

  constructor(options: Partial<CliAdapterOptions> = {}) {
    super({ ...options, binary: options.binary ?? process.env.VTX_OMP_BIN ?? "omp" });
  }

  protected buildArgs(prompt: string, workspacePath: string): string[] {
    return ["--mode", "json", "-p", "--no-session", "--cwd", workspacePath, prompt];
  }
}
