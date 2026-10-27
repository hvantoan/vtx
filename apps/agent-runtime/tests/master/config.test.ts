import { join } from "node:path";
import { describe, expect, it } from "vitest";

import { DEFAULT_INTERVAL_MS, DEFAULT_JQL, loadMasterConfig } from "../../src/master/config.js";

describe("loadMasterConfig", () => {
  it("derives paths from the repository root", () => {
    const config = loadMasterConfig({}, []);
    expect(config.repoRoot).toMatch(/vtx$/);
    expect(config.mcpConfigPath).toBe(join(config.repoRoot, ".pi/mcp.json"));
    expect(config.stateDir).toBe(join(config.repoRoot, "var/master"));
    expect(config.jql).toBe(DEFAULT_JQL);
    expect(config.intervalMs).toBe(DEFAULT_INTERVAL_MS);
    expect(config.baseline).toBe(true);
    expect(config.deliverIntake).toBe(true);
    expect(config.spineSocketPath).toBe(join(config.repoRoot, "var/run/vtx-spine.sock"));
    expect(config.spineToken).toBe("");
    expect(config.once).toBe(false);
  });

  it("honours environment overrides and --once", () => {
    const config = loadMasterConfig(
      {
        VTX_JIRA_POLL_INTERVAL_MS: "60000",
        VTX_MASTER_BASELINE: "0",
        VTX_JIRA_JQL: "project = VTX AND status = Done",
        VTX_MASTER_STATE_DIR: "/tmp/vtx-master",
        VTX_JIRA_MCP_CONFIG: "/tmp/mcp.json",
        VTX_JIRA_MCP_SERVER: "custom",
        VTX_JIRA_PAGE_SIZE: "10",
        VTX_MASTER_DELIVER: "0",
        SPINE_SOCKET_PATH: "/tmp/spine.sock",
        SPINE_AUTH_TOKEN: "tok",
      },
      ["--once"],
    );
    expect(config.intervalMs).toBe(60_000);
    expect(config.baseline).toBe(false);
    expect(config.jql).toBe("project = VTX AND status = Done");
    expect(config.stateDir).toBe("/tmp/vtx-master");
    expect(config.mcpConfigPath).toBe("/tmp/mcp.json");
    expect(config.serverName).toBe("custom");
    expect(config.pageSize).toBe(10);
    expect(config.deliverIntake).toBe(false);
    expect(config.spineSocketPath).toBe("/tmp/spine.sock");
    expect(config.spineToken).toBe("tok");
    expect(config.once).toBe(true);
  });

  it("rejects an out-of-range interval instead of spinning", () => {
    const config = loadMasterConfig({ VTX_JIRA_POLL_INTERVAL_MS: "0" }, []);
    expect(config.intervalMs).toBe(DEFAULT_INTERVAL_MS);
    const tooLarge = loadMasterConfig({ VTX_JIRA_POLL_INTERVAL_MS: "999999999" }, []);
    expect(tooLarge.intervalMs).toBe(DEFAULT_INTERVAL_MS);
  });
});
