import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const DEFAULT_JQL = 'project = VTX AND status = "Ready To Develope" ORDER BY updated ASC';
export const DEFAULT_INTERVAL_MS = 30 * 60 * 1000;

export interface MasterConfig {
  repoRoot: string;
  mcpConfigPath: string;
  serverName: string;
  jql: string;
  intervalMs: number;
  pageSize: number;
  stateDir: string;
  /** Mark currently matching issues as seen on the first tick instead of queueing them. */
  baseline: boolean;
  /** Deliver queued entries to the spine intake tool (needs spineToken). */
  deliverIntake: boolean;
  spineSocketPath: string;
  spineToken: string;
  once: boolean;
}

function positiveInt(value: string | undefined, fallback: number, min: number, max: number): number {
  if (value === undefined || value.trim() === "") return fallback;
  const parsed = Number.parseInt(value, 10);
  if (!Number.isFinite(parsed) || parsed < min || parsed > max) return fallback;
  return parsed;
}

function repoRootFromModule(moduleUrl: string): string {
  // src/master/config.ts or dist/master/config.js -> repository root
  return resolve(dirname(fileURLToPath(moduleUrl)), "../../../..");
}

export function loadMasterConfig(env: NodeJS.ProcessEnv = process.env, argv: string[] = process.argv.slice(2)): MasterConfig {
  const repoRoot = env.VTX_REPO_ROOT?.trim() || repoRootFromModule(import.meta.url);
  return {
    repoRoot,
    mcpConfigPath: env.VTX_JIRA_MCP_CONFIG?.trim() || resolve(repoRoot, ".pi/mcp.json"),
    serverName: env.VTX_JIRA_MCP_SERVER?.trim() || "vtx-jira",
    jql: env.VTX_JIRA_JQL?.trim() || DEFAULT_JQL,
    intervalMs: positiveInt(env.VTX_JIRA_POLL_INTERVAL_MS, DEFAULT_INTERVAL_MS, 1000, 24 * 60 * 60 * 1000),
    pageSize: positiveInt(env.VTX_JIRA_PAGE_SIZE, 50, 1, 50),
    stateDir: env.VTX_MASTER_STATE_DIR?.trim() || resolve(repoRoot, "var/master"),
    baseline: env.VTX_MASTER_BASELINE !== "0",
    deliverIntake: env.VTX_MASTER_DELIVER !== "0",
    spineSocketPath: env.SPINE_SOCKET_PATH?.trim() || resolve(repoRoot, "var/run/vtx-spine.sock"),
    spineToken: env.SPINE_AUTH_TOKEN ?? "",
    once: argv.includes("--once"),
  };
}
