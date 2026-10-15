package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vtx/platform/internal/config"
)

// jiraEnv clears every Jira intake env key the loader inspects, seeds the
// other required envs so Validate() reaches the intake checks, then applies
// the test's overrides.
func jiraEnv(t *testing.T, overrides map[string]string) {
	t.Helper()
	for _, k := range []string{
		"JIRA_CONFIG_PATH", "JIRA_PROJECT_ALLOWLIST",
		"JIRA_READY_TO_DEV_STATUS_ID", "JIRA_SITE_ID",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	base := map[string]string{
		"JIRA_BASE_URL":               "https://vtxsolution.atlassian.net",
		"JIRA_API_EMAIL":              "bot@example.com",
		"JIRA_API_TOKEN":              "token",
		"GATEWAY_INTERNAL_AUTH_TOKEN": "gw-token",
	}
	for k, v := range base {
		t.Setenv(k, v)
	}
	for k, v := range overrides {
		t.Setenv(k, v)
	}
}

func writeJiraYAML(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "jira.yaml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

const validJiraYAML = `jira:
  projects:
    VTX:
      start_status_ids: ["10007", "10008"]
`

func TestJiraYAMLHappyPath(t *testing.T) {
	jiraEnv(t, map[string]string{
		"JIRA_CONFIG_PATH": writeJiraYAML(t, validJiraYAML),
		"JIRA_SITE_ID":     "vtxsolution.atlassian.net",
	})
	cfg := config.Load()

	ids, ok := cfg.StartStatusIDsFor("VTX")
	assert.True(t, ok)
	assert.Equal(t, []string{"10007", "10008"}, ids)
	assert.Equal(t, "yaml", cfg.JiraConfigSource())
	assert.Equal(t, []string{"VTX"}, cfg.JiraProjectKeys())

	_, ok = cfg.StartStatusIDsFor("OTHER")
	assert.False(t, ok, "unknown project must not resolve an allowlist")
}

func TestDualControlFails(t *testing.T) {
	for _, legacy := range []map[string]string{
		{"JIRA_READY_TO_DEV_STATUS_ID": "10007"},
		{"JIRA_PROJECT_ALLOWLIST": "VTX"},
		{"JIRA_PROJECT_ALLOWLIST": "VTX", "JIRA_READY_TO_DEV_STATUS_ID": "10007"},
	} {
		legacy["JIRA_CONFIG_PATH"] = writeJiraYAML(t, validJiraYAML)
		jiraEnv(t, legacy)
		err := config.Load().Validate()
		require.Error(t, err, "legacy env + JIRA_CONFIG_PATH must be fatal")
		assert.Contains(t, err.Error(), "JIRA_CONFIG_PATH")
		assert.Contains(t, err.Error(), "single source of truth")
	}
}

func TestSiteIDDualControlFails(t *testing.T) {
	path := writeJiraYAML(t, `jira:
  site_id: "yaml-site.atlassian.net"
  projects:
    VTX: {start_status_ids: ["10007"]}
`)
	jiraEnv(t, map[string]string{
		"JIRA_CONFIG_PATH": path,
		"JIRA_SITE_ID":     "vtxsolution.atlassian.net",
	})
	err := config.Load().Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JIRA_SITE_ID")
}

func TestYAMLSiteIDResolves(t *testing.T) {
	path := writeJiraYAML(t, `jira:
  site_id: "yaml-site.atlassian.net"
  projects:
    VTX: {start_status_ids: ["10007"]}
`)
	jiraEnv(t, map[string]string{"JIRA_CONFIG_PATH": path})
	cfg := config.Load()
	assert.Equal(t, "yaml-site.atlassian.net", cfg.JiraSiteID)
	assert.NoError(t, cfg.Validate())
}

func TestStrictValidationFailures(t *testing.T) {
	cases := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{"unknown top key", "jira:\n  projects:\n    VTX: {start_status_ids: [\"10007\"]}\nbogus: 1\n", "bogus"},
		{"unknown nested key", "jira:\n  projects:\n    VTX: {start_status_ids: [\"10007\"], typo_key: 1}\n", "typo_key"},
		{"empty projects", "jira:\n  projects: {}\n", "at least one project"},
		{"empty status list", "jira:\n  projects:\n    VTX: {start_status_ids: []}\n", "must not be empty"},
		{"non-numeric ID", "jira:\n  projects:\n    VTX: {start_status_ids: [\"ready\"]}\n", "not a numeric"},
		{"duplicate ID", "jira:\n  projects:\n    VTX: {start_status_ids: [\"10007\", \"10007\"]}\n", "duplicate"},
		{"bad project key", "jira:\n  projects:\n    vtx: {start_status_ids: [\"10007\"]}\n", "must match"},
		{"missing file sentinel", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.yaml
			if tc.name == "missing file sentinel" {
				path = filepath.Join(t.TempDir(), "does-not-exist.yaml")
			} else {
				path = writeJiraYAML(t, tc.yaml)
			}
			jiraEnv(t, map[string]string{
				"JIRA_CONFIG_PATH": path,
				"JIRA_SITE_ID":     "s.atlassian.net",
			})
			err := config.Load().Validate()
			require.Error(t, err)
			if tc.wantErr != "" {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLegacyEnvFallback(t *testing.T) {
	jiraEnv(t, map[string]string{
		"JIRA_PROJECT_ALLOWLIST":      "VTX, OPS",
		"JIRA_READY_TO_DEV_STATUS_ID": "10007",
		"JIRA_SITE_ID":                "vtxsolution.atlassian.net",
	})
	cfg := config.Load()

	assert.Equal(t, "env", cfg.JiraConfigSource())
	ids, ok := cfg.StartStatusIDsFor("VTX")
	assert.True(t, ok)
	assert.Equal(t, []string{"10007"}, ids)
	_, ok = cfg.StartStatusIDsFor("OPS")
	assert.True(t, ok)
	assert.ElementsMatch(t, []string{"OPS", "VTX"}, cfg.JiraProjectKeys())
}

func TestMissingIntakeConfigFails(t *testing.T) {
	jiraEnv(t, nil)
	err := config.Load().Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "JIRA_CONFIG_PATH")
}
