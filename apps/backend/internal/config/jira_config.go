package config

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// JiraProjectConfig is the per-project intake policy: which status IDs count
// as "start" statuses whose transitions produce an execution intent.
type JiraProjectConfig struct {
	StartStatusIDs []string `yaml:"start_status_ids"`
}

// JiraFileConfig mirrors the JIRA_CONFIG_PATH YAML file. When the path is set
// the file is the single source of truth for project→status mappings — the
// legacy env keys JIRA_PROJECT_ALLOWLIST / JIRA_READY_TO_DEV_STATUS_ID must be
// unset, never silently merged.
type JiraFileConfig struct {
	Jira struct {
		SiteID   string                       `yaml:"site_id"`
		Projects map[string]JiraProjectConfig `yaml:"projects"`
	} `yaml:"jira"`
}

var (
	projectKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]+$`)
	statusIDRe   = regexp.MustCompile(`^[0-9]+$`)
)

// JiraConfigSource reports where Jira intake config came from ("yaml" | "env").
func (c *Config) JiraConfigSource() string {
	if c.JiraConfigPath != "" {
		return "yaml"
	}
	return "env"
}

// StartStatusIDsFor returns the configured start-status allowlist for a
// project. Unknown projects get (nil, false) — a hard scope gate, never an
// empty default that silently allows or denies.
func (c *Config) StartStatusIDsFor(projectKey string) ([]string, bool) {
	p, ok := c.JiraProjects[projectKey]
	if !ok {
		return nil, false
	}
	return p.StartStatusIDs, true
}

// JiraProjectKeys returns the sorted allowlisted project keys — for startup
// audit logs and diagnostics. IDs and keys are not secrets.
func (c *Config) JiraProjectKeys() []string {
	keys := make([]string, 0, len(c.JiraProjects))
	for k := range c.JiraProjects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadJiraFile reads and strictly validates the YAML config. Fail-closed: any
// structural or semantic problem is a fatal configuration error.
func loadJiraFile(path string) (*JiraFileConfig, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read JIRA_CONFIG_PATH %q: %w", path, err)
	}
	defer f.Close()

	var fc JiraFileConfig
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&fc); err != nil {
		return nil, fmt.Errorf("parse JIRA_CONFIG_PATH %q: %w", path, err)
	}
	if len(fc.Jira.Projects) == 0 {
		return nil, fmt.Errorf("JIRA_CONFIG_PATH %q: jira.projects must name at least one project", path)
	}
	for key, proj := range fc.Jira.Projects {
		if err := validateProjectEntry(key, proj); err != nil {
			return nil, fmt.Errorf("JIRA_CONFIG_PATH %q: %w", path, err)
		}
	}
	return &fc, nil
}

// validateProjectEntry enforces the per-project invariants shared by the YAML
// path and the env-derived fallback.
func validateProjectEntry(key string, proj JiraProjectConfig) error {
	if !projectKeyRe.MatchString(key) {
		return fmt.Errorf("project key %q must match ^[A-Z][A-Z0-9_]+$", key)
	}
	if len(proj.StartStatusIDs) == 0 {
		return fmt.Errorf("projects.%s.start_status_ids must not be empty", key)
	}
	seen := map[string]bool{}
	for _, id := range proj.StartStatusIDs {
		if !statusIDRe.MatchString(id) {
			return fmt.Errorf("projects.%s.start_status_ids entry %q is not a numeric status ID", key, id)
		}
		if seen[id] {
			return fmt.Errorf("projects.%s.start_status_ids has duplicate ID %q", key, id)
		}
		seen[id] = true
	}
	return nil
}

// loadJiraIntake resolves the single source of truth for project→status
// mappings. Call from Load() AFTER env vars are read; errors are stored on the
// Config and surfaced by Validate() so startup fails with the full context.
func (c *Config) loadJiraIntake() {
	yamlSet := c.JiraConfigPath != ""
	legacyAllowlist := len(c.JiraProjectAllowlist) > 0
	legacyStatus := c.JiraLegacyReadyStatus != ""

	if yamlSet && (legacyAllowlist || legacyStatus) {
		var keys []string
		if legacyAllowlist {
			keys = append(keys, "JIRA_PROJECT_ALLOWLIST")
		}
		if legacyStatus {
			keys = append(keys, "JIRA_READY_TO_DEV_STATUS_ID")
		}
		c.jiraConfigErr = fmt.Errorf(
			"JIRA_CONFIG_PATH is set but legacy key(s) %s are also set — "+
				"the YAML file is the single source of truth; remove the env keys (no silent merge)",
			strings.Join(keys, ", "))
		return
	}

	if yamlSet {
		fc, err := loadJiraFile(c.JiraConfigPath)
		if err != nil {
			c.jiraConfigErr = err
			return
		}
		c.JiraProjects = fc.Jira.Projects
		// site_id may move into YAML, but never from both places at once.
		if fc.Jira.SiteID != "" && c.JiraSiteID != "" {
			c.jiraConfigErr = fmt.Errorf(
				"jira.site_id is set in %q and JIRA_SITE_ID is also set — pick one source (no silent merge)",
				c.JiraConfigPath)
			return
		}
		if fc.Jira.SiteID != "" {
			c.JiraSiteID = fc.Jira.SiteID
		}
		return
	}

	// Backward-compat window: translate legacy env keys into the same model so
	// downstream code sees exactly one shape.
	if legacyAllowlist || legacyStatus {
		c.JiraProjects = map[string]JiraProjectConfig{}
		for _, key := range c.JiraProjectAllowlist {
			ids := []string{}
			if c.JiraLegacyReadyStatus != "" {
				ids = []string{c.JiraLegacyReadyStatus}
			}
			c.JiraProjects[key] = JiraProjectConfig{StartStatusIDs: ids}
		}
	}
}

// validateJiraProjects enforces fail-closed invariants on the resolved model
// (both YAML and env-derived paths land here).
func (c *Config) validateJiraProjects() error {
	if len(c.JiraProjects) == 0 {
		return fmt.Errorf("jira intake config required: set JIRA_CONFIG_PATH (YAML) or JIRA_PROJECT_ALLOWLIST + JIRA_READY_TO_DEV_STATUS_ID (legacy env)")
	}
	for key, proj := range c.JiraProjects {
		if err := validateProjectEntry(key, proj); err != nil {
			return err
		}
	}
	return nil
}
