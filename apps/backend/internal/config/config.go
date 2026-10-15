package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port              string
	Host              string
	Env               string
	AuthToken         string
	DatabaseURL       string
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	TemporalHostPort  string
	TemporalNamespace string
	TemporalQueue     string
	AgentRuntimeDir   string
	AgentTaskTimeout  time.Duration

	// Jira webhook intake (VTX-2)
	JiraBaseURL              string
	JiraAPIEmail             string
	JiraAPIToken             string
	JiraProjectAllowlist     []string
	JiraReadyToDevStatusID   string
	JiraSiteID               string
	GatewayInternalAuthToken string
}

func Load() *Config {
	return &Config{
		Port:              getEnv("PORT", "49080"),
		Host:              getEnv("API_HOST", "0.0.0.0"),
		Env:               getEnv("APP_ENV", "development"),
		AuthToken:         getEnv("API_AUTH_TOKEN", "vtx-dev-secret-token-change-in-production"),
		DatabaseURL:       getEnv("DATABASE_URL", "postgres://vtx_user:vtx_password@localhost:49432/vtx_platform?sslmode=disable"),
		DBMaxOpenConns:    getEnvAsInt("DB_MAX_OPEN_CONNS", 25),
		DBMaxIdleConns:    getEnvAsInt("DB_MAX_IDLE_CONNS", 5),
		DBConnMaxLifetime: getEnvAsDuration("DB_CONN_MAX_LIFETIME", 15*time.Minute),
		TemporalHostPort:  getEnv("TEMPORAL_HOST_PORT", "localhost:49233"),
		TemporalNamespace: getEnv("TEMPORAL_NAMESPACE", "default"),
		TemporalQueue:     getEnv("TEMPORAL_TASK_QUEUE", "vtx-task-queue"),
		AgentRuntimeDir:   getEnv("AGENT_RUNTIME_DIR", "./apps/agent-runtime"),
		AgentTaskTimeout:  getEnvAsDuration("AGENT_TASK_TIMEOUT", 60*time.Second),

		JiraBaseURL:              getEnv("JIRA_BASE_URL", ""),
		JiraAPIEmail:             getEnv("JIRA_API_EMAIL", ""),
		JiraAPIToken:             getEnv("JIRA_API_TOKEN", ""),
		JiraProjectAllowlist:     splitCSV(getEnv("JIRA_PROJECT_ALLOWLIST", "")),
		JiraReadyToDevStatusID:   getEnv("JIRA_READY_TO_DEV_STATUS_ID", ""),
		JiraSiteID:               getEnv("JIRA_SITE_ID", ""),
		GatewayInternalAuthToken: getEnv("GATEWAY_INTERNAL_AUTH_TOKEN", ""),
	}
}

// Validate enforces fail-closed startup invariants for the webhook intake.
// Tokens carry no insecure defaults: an unset or colliding internal token is a
// fatal configuration error, never a fallback to the public dashboard token.
func (c *Config) Validate() error {
	if c.GatewayInternalAuthToken == "" {
		return fmt.Errorf("GATEWAY_INTERNAL_AUTH_TOKEN is required and must not be empty")
	}
	if c.GatewayInternalAuthToken == c.AuthToken {
		return fmt.Errorf("GATEWAY_INTERNAL_AUTH_TOKEN must differ from API_AUTH_TOKEN (the public dashboard token)")
	}
	for _, missing := range requiredJira(c) {
		return fmt.Errorf("%s is required for the Jira webhook intake", missing)
	}
	if !strings.HasPrefix(c.JiraBaseURL, "https://") && !strings.HasPrefix(c.JiraBaseURL, "http://") {
		return fmt.Errorf("JIRA_BASE_URL must be an http(s) URL (got %q)", c.JiraBaseURL)
	}
	return nil
}

func requiredJira(c *Config) []string {
	var missing []string
	if c.JiraBaseURL == "" {
		missing = append(missing, "JIRA_BASE_URL")
	}
	if c.JiraAPIEmail == "" {
		missing = append(missing, "JIRA_API_EMAIL")
	}
	if c.JiraAPIToken == "" {
		missing = append(missing, "JIRA_API_TOKEN")
	}
	if len(c.JiraProjectAllowlist) == 0 {
		missing = append(missing, "JIRA_PROJECT_ALLOWLIST")
	}
	if c.JiraReadyToDevStatusID == "" {
		missing = append(missing, "JIRA_READY_TO_DEV_STATUS_ID")
	}
	if c.JiraSiteID == "" {
		missing = append(missing, "JIRA_SITE_ID")
	}
	return missing
}

func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func splitCSV(val string) []string {
	var out []string
	for _, part := range strings.Split(val, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

func getEnvAsInt(key string, defaultVal int) int {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return defaultVal
	}
	return val
}

func getEnvAsDuration(key string, defaultVal time.Duration) time.Duration {
	valStr := os.Getenv(key)
	if valStr == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(valStr)
	if err != nil {
		return defaultVal
	}
	return d
}
