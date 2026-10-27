package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/vtx/platform/internal/domain"
)

type Config struct {
	Port              string
	Host              string
	Env               string
	DatabaseURL       string
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime time.Duration
	TemporalHostPort  string
	TemporalNamespace string
	TemporalQueue     string
	AgentRuntimeDir   string
	AgentTaskTimeout  time.Duration

	// Jira webhook intake (VTX-2). The project→start-status mapping resolves
	// into JiraProjects from exactly one source: JIRA_CONFIG_PATH (YAML) or
	// the legacy JIRA_PROJECT_ALLOWLIST × JIRA_READY_TO_DEV_STATUS_ID env
	// pair. Both set → fatal (no dual control, no silent merge).
	JiraBaseURL              string
	JiraAPIEmail             string
	JiraAPIToken             string
	JiraConfigPath           string
	JiraProjects             map[string]JiraProjectConfig
	JiraSiteID               string
	GatewayInternalAuthToken string

	// App-hosted operator login. SessionTTL is the sliding session lifetime;
	// SessionCookieSecure is "auto" (Secure when the request is HTTPS,
	// including a TLS-terminating proxy), "always", or "never".
	SessionTTL          time.Duration
	SessionCookieSecure string

	// RegistryConsumerName is the delivery identity of the VTX-3 outbox
	// dispatcher (all its replicas). Actor tokens own the HTTP API surface; the
	// dispatcher authenticates as this consumer, not as an actor.
	RegistryConsumerName string

	// RegistryActorTokens binds each authenticated principal to its own bearer
	// token (D5). The actor a request acts as comes from here — never from the
	// request body.
	RegistryActorTokens map[domain.Actor]string

	// VTX-4 worktree lifecycle. The three roots are the canonical layout the
	// service proves every fs path against; nothing outside them is touched.
	WorktreeRoot           string
	WorktreeQuarantineRoot string
	RepoCacheRoot          string
	WorktreeLeaseTTL       time.Duration
	WorktreeReconcileEvery time.Duration
	// WorktreeOwner is the lease_owner identity this process writes to
	// task_runs (e.g. "worker-1", "scheduler"). Empty → service default.
	WorktreeOwner string

	// VTX-12 draft PR integration.
	GitHubToken         string
	GitHubAPIBaseURL    string
	GitHubRepoAllowlist []string
	PRBaseBranch        string

	// VTX-13 operator measurements.
	WorkerHeartbeatInterval time.Duration
	WorkerStaleFactor       int

	// VTX-ORCH orchestration launcher (`worker orchestrate`). Paths are
	// resolved against the CWD (the repository root, same as `make master`).
	OrchestratorPiBinary       string
	OrchestratorExtension      string
	OrchestratorSpecDir        string
	OrchestratorLogDir         string
	OrchestratorMaxParallel    int
	OrchestratorMaxTurns       int
	OrchestratorMaxTokens      int
	OrchestratorWallClock      time.Duration
	OrchestratorMaxDelegations int
	OrchestratorClaimStale     time.Duration
	OrchestratorLeaseRenew     time.Duration

	// VTX-ORCH spine tool server (Phase 1). The Pi orchestrator extension
	// connects to SpineSocketPath and authenticates each request with
	// SpineAuthToken. An empty token disables the `worker spine` mode (fail
	// closed) — the spine is a credential boundary, never unauthenticated.
	SpineSocketPath string
	SpineAuthToken  string

	// Legacy env fallback inputs — read in Load(), consumed by
	// loadJiraIntake(), never read by downstream code.
	JiraProjectAllowlist  []string
	JiraLegacyReadyStatus string
	jiraConfigErr         error
}

func Load() *Config {
	c := &Config{
		Port:              getEnv("PORT", "49080"),
		Host:              getEnv("API_HOST", "0.0.0.0"),
		Env:               getEnv("APP_ENV", "development"),
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
		JiraConfigPath:           getEnv("JIRA_CONFIG_PATH", ""),
		JiraProjectAllowlist:     splitCSV(getEnv("JIRA_PROJECT_ALLOWLIST", "")),
		JiraLegacyReadyStatus:    getEnv("JIRA_READY_TO_DEV_STATUS_ID", ""),
		JiraSiteID:               getEnv("JIRA_SITE_ID", ""),
		GatewayInternalAuthToken: getEnv("GATEWAY_INTERNAL_AUTH_TOKEN", ""),

		// App-hosted operator login.
		SessionTTL:          getEnvAsDuration("SESSION_TTL", 720*time.Hour),
		SessionCookieSecure: getEnv("SESSION_COOKIE_SECURE", "auto"),

		// VTX-3 registry intake. The consumer name is the delivery identity
		// that owns outbox ACKs; all replicas of the dispatcher share it.
		RegistryConsumerName: getEnv("REGISTRY_CONSUMER_NAME", "registry-intake"),

		// VTX-4 worktree lifecycle.
		WorktreeRoot:            getEnv("WORKTREE_ROOT", "./var/worktrees"),
		WorktreeQuarantineRoot:  getEnv("WORKTREE_QUARANTINE_ROOT", "./var/worktrees-quarantine"),
		RepoCacheRoot:           getEnv("REPO_CACHE_ROOT", "./var/repo-cache"),
		WorktreeLeaseTTL:        getEnvAsDuration("WORKTREE_LEASE_TTL", 2*time.Minute),
		WorktreeReconcileEvery:  getEnvAsDuration("WORKTREE_RECONCILE_INTERVAL", 5*time.Minute),
		WorktreeOwner:           getEnv("WORKTREE_OWNER", ""),
		GitHubToken:             getEnv("GITHUB_TOKEN", ""),
		GitHubAPIBaseURL:        getEnv("GITHUB_API_BASE_URL", "https://api.github.com"),
		GitHubRepoAllowlist:     splitCSV(getEnv("GITHUB_REPO_ALLOWLIST", "")),
		PRBaseBranch:            getEnv("PR_BASE_BRANCH", "dev"),
		WorkerHeartbeatInterval: getEnvAsDuration("WORKER_HEARTBEAT_INTERVAL", 15*time.Second),
		WorkerStaleFactor:       getEnvAsInt("WORKER_STALE_FACTOR", 3),

		OrchestratorPiBinary:       getEnv("ORCHESTRATOR_PI_BINARY", "apps/agent-runtime/node_modules/.bin/pi"),
		OrchestratorExtension:      getEnv("ORCHESTRATOR_EXTENSION", "apps/agent-runtime/src/orchestrator/vtx-orchestrator.ts"),
		OrchestratorSpecDir:        getEnv("ORCHESTRATOR_SPEC_DIR", "./var/run/specs"),
		OrchestratorLogDir:         getEnv("ORCHESTRATOR_LOG_DIR", "./var/run/agent-logs"),
		OrchestratorMaxParallel:    getEnvAsInt("ORCHESTRATOR_MAX_PARALLEL", 3),
		OrchestratorMaxTurns:       getEnvAsInt("ORCHESTRATOR_MAX_TURNS", 60),
		OrchestratorMaxTokens:      getEnvAsInt("ORCHESTRATOR_MAX_TOKENS", 200000),
		OrchestratorWallClock:      getEnvAsDuration("ORCHESTRATOR_WALL_CLOCK", 2*time.Hour),
		OrchestratorMaxDelegations: getEnvAsInt("ORCHESTRATOR_MAX_DELEGATIONS", 12),
		OrchestratorClaimStale:     getEnvAsDuration("ORCHESTRATOR_CLAIM_STALE", 2*time.Minute),
		OrchestratorLeaseRenew:     getEnvAsDuration("ORCHESTRATOR_LEASE_RENEW", 30*time.Second),
		SpineSocketPath:            getEnv("SPINE_SOCKET_PATH", "./var/run/vtx-spine.sock"),
		SpineAuthToken:             getEnv("SPINE_AUTH_TOKEN", ""),
		RegistryActorTokens: map[domain.Actor]string{
			domain.ActorScheduler:    getEnv("REGISTRY_ACTOR_SCHEDULER_TOKEN", ""),
			domain.ActorOrchestrator: getEnv("REGISTRY_ACTOR_ORCHESTRATOR_TOKEN", ""),
			domain.ActorPolicy:       getEnv("REGISTRY_ACTOR_POLICY_TOKEN", ""),
		},
	}
	c.loadJiraIntake()
	return c
}

// Validate enforces fail-closed startup invariants for the webhook intake.
// Tokens carry no insecure defaults: an unset or colliding internal token is a
// fatal configuration error, never a fallback to the public dashboard token.
func (c *Config) Validate() error {
	if c.GatewayInternalAuthToken == "" {
		return fmt.Errorf("GATEWAY_INTERNAL_AUTH_TOKEN is required and must not be empty")
	}
	switch strings.ToLower(c.SessionCookieSecure) {
	case "auto", "always", "never":
	default:
		return fmt.Errorf("SESSION_COOKIE_SECURE must be auto, always or never (got %q)", c.SessionCookieSecure)
	}
	if c.SessionTTL <= 0 {
		return fmt.Errorf("SESSION_TTL must be positive (got %s)", c.SessionTTL)
	}
	if c.jiraConfigErr != nil {
		return c.jiraConfigErr
	}
	for _, missing := range requiredJira(c) {
		return fmt.Errorf("%s is required for the Jira webhook intake", missing)
	}
	if !strings.HasPrefix(c.JiraBaseURL, "https://") && !strings.HasPrefix(c.JiraBaseURL, "http://") {
		return fmt.Errorf("JIRA_BASE_URL must be an http(s) URL (got %q)", c.JiraBaseURL)
	}
	if err := c.validateJiraProjects(); err != nil {
		return err
	}
	return nil
}

// ValidateJiraIntake checks the Jira fields the poll-intake path needs. Unlike
// Validate it does not require gateway tokens, so the spine tool server can
// expose intake without the webhook's transport secret.
func (c *Config) ValidateJiraIntake() error {
	if c.jiraConfigErr != nil {
		return c.jiraConfigErr
	}
	if missing := requiredJira(c); len(missing) > 0 {
		return fmt.Errorf("%s is required for the Jira poll intake", missing[0])
	}
	if !strings.HasPrefix(c.JiraBaseURL, "https://") && !strings.HasPrefix(c.JiraBaseURL, "http://") {
		return fmt.Errorf("JIRA_BASE_URL must be an http(s) URL (got %q)", c.JiraBaseURL)
	}
	return c.validateJiraProjects()
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
	if len(c.JiraProjects) == 0 {
		missing = append(missing, "JIRA_CONFIG_PATH or JIRA_PROJECT_ALLOWLIST+JIRA_READY_TO_DEV_STATUS_ID")
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
