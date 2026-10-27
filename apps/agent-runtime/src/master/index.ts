import { loadMasterConfig } from "./config.js";
import { connectSpine, drainQueue, queueCursorPath, spineIntakeDeliver } from "./delivery.js";
import { startPollLoop } from "./loop.js";
import { McpJiraSource } from "./mcp-jira.js";
import { pollOnce } from "./poller.js";
import { queuePath } from "./state.js";

function log(event: Record<string, unknown>): void {
  console.log(JSON.stringify({ ts: new Date().toISOString(), ...event }));
}

async function main(): Promise<void> {
  const config = loadMasterConfig();
  const source = await McpJiraSource.connect(config.mcpConfigPath, config.serverName);

  const deliverQueued = async (): Promise<void> => {
    if (!config.deliverIntake) return;
    if (config.spineToken === "") {
      log({ event: "jira_intake_disabled", reason: "SPINE_AUTH_TOKEN unset; queue retained for replay" });
      return;
    }
    let client;
    try {
      client = await connectSpine(config.spineSocketPath, config.spineToken);
    } catch (error) {
      log({
        event: "jira_intake_unreachable",
        socket: config.spineSocketPath,
        error: error instanceof Error ? error.message : String(error),
      });
      return;
    }
    try {
      await drainQueue({
        queuePath: queuePath(config.stateDir),
        cursorPath: queueCursorPath(config.stateDir),
        deliver: spineIntakeDeliver(client),
        log,
        now: () => new Date(),
      });
    } finally {
      client.close();
    }
  };

  const runOnce = async (): Promise<void> => {
    await pollOnce({
      source,
      stateDir: config.stateDir,
      jql: config.jql,
      pageSize: config.pageSize,
      baseline: config.baseline,
      now: () => new Date(),
      log,
    });
    await deliverQueued();
  };

  if (config.once) {
    try {
      await runOnce();
    } finally {
      await source.close();
    }
    return;
  }

  log({
    event: "jira_master_start",
    jql: config.jql,
    intervalMs: config.intervalMs,
    stateDir: config.stateDir,
    baseline: config.baseline,
    deliverIntake: config.deliverIntake,
    spineSocketPath: config.spineSocketPath,
  });

  const loop = startPollLoop({ intervalMs: config.intervalMs, runOnce, log });
  let shuttingDown = false;
  const shutdown = (signal: string): void => {
    if (shuttingDown) return;
    shuttingDown = true;
    log({ event: "jira_master_stop", signal });
    loop.stop();
    void source.close().finally(() => process.exit(0));
  };
  process.on("SIGINT", () => shutdown("SIGINT"));
  process.on("SIGTERM", () => shutdown("SIGTERM"));
}

main().catch((error: unknown) => {
  log({
    event: "jira_master_fatal",
    error: error instanceof Error ? error.message : String(error),
  });
  process.exitCode = 1;
});
