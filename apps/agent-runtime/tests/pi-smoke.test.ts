import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { execFile } from "node:child_process";
import { readdir, realpath, rm } from "node:fs/promises";
import { promisify } from "node:util";
import { PiCliAdapter } from "../src/adapters/pi.js";
import type { AdapterEvent } from "../src/adapters/types.js";
import { makeTempDir } from "./support/host-harness.js";

const execFileAsync = promisify(execFile);

/**
 * REAL Pi smoke test (AGT-14). Opt-in only: it calls the real `pi` binary and a real model,
 * so it stays skipped in CI and default unit runs. Enable with `VTX_SMOKE_PI=1`.
 * Optional: `VTX_PI_BIN` (binary path), `VTX_SMOKE_PI_ARGS` (extra CLI flags, whitespace separated, e.g. `--model x`).
 */
const smokeEnabled = process["env"].VTX_SMOKE_PI === "1";

describe.skipIf(!smokeEnabled)("real pi CLI smoke (VTX_SMOKE_PI=1)", () => {
  let worktree: string;

  const git = async (...args: string[]): Promise<string> => {
    const { stdout } = await execFileAsync("git", args, { cwd: worktree });
    return stdout.trim();
  };

  beforeEach(async () => {
    worktree = await realpath(await makeTempDir("vtx-pi-smoke-"));
    await git("init", "-q", "-b", "main");
    await git("-c", "user.email=t@example.com", "-c", "user.name=T", "commit", "-q", "--allow-empty", "-m", "base");
  });

  afterEach(async () => {
    await rm(worktree, { recursive: true, force: true });
  });

  it("executes in a git worktree, produces a commit, and reports test_double=false", async () => {
    const extraArgs = (process["env"].VTX_SMOKE_PI_ARGS ?? "").split(/\s+/).filter(Boolean);
    const adapter = new PiCliAdapter({ extraArgs });
    const env = { PATH: process["env"].PATH, HOME: process["env"].HOME };
    await adapter.preflight(env);
    const baseSha = await git("rev-parse", "HEAD");

    const events: AdapterEvent[] = [];
    const result = await adapter.execute({
      params: {
        task_id: "smoke-1",
        prompt: "Create a file named hello.txt containing exactly the word hello, then commit it with git. Do nothing else.",
        workspace_path: worktree,
      },
      workspacePath: worktree,
      env,
      onEvent: (event) => events.push(event),
      signal: AbortSignal.timeout(240_000),
    });

    expect(result.status).toBe("succeeded");
    expect(result.test_double).toBe(false);
    expect(await readdir(worktree)).toContain("hello.txt");
    const headSha = await git("rev-parse", "HEAD");
    expect(headSha).not.toBe(baseSha);
    expect(result.artifacts).toEqual([{ kind: "commit", ref: headSha }]);
    expect(events.some((e) => e.event_type === "tool_call")).toBe(true);
  }, 300_000);
});
