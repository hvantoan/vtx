import { CliAgentAdapter } from "./cli.js";
import type { CliAdapterOptions } from "./cli.js";

/** Primary real adapter: shells out to the `pi` CLI in non-interactive JSON mode. */
export class PiCliAdapter extends CliAgentAdapter {
  readonly name = "pi" as const;

  constructor(options: Partial<CliAdapterOptions> = {}) {
    super({ ...options, binary: options.binary ?? process.env.VTX_PI_BIN ?? "pi" });
  }

  protected buildArgs(prompt: string): string[] {
    return ["--mode", "json", "-p", "--no-session", prompt];
  }
}
