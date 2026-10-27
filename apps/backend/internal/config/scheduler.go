package config

import (
	"os"
	"strings"
	"time"
)

type SchedulerSettings struct {
	Owner                        string
	Tick, LeaseTTL, BeatInterval time.Duration
	JiraGuard                    bool
}
type RequirementSettings struct {
	Command           string
	Timeout, LeaseTTL time.Duration
}

func Scheduler() SchedulerSettings {
	owner := getEnv("SCHEDULER_OWNER", "")
	if owner == "" {
		host, err := os.Hostname()
		if err == nil {
			owner = "scheduler-" + host
		}
	}
	return SchedulerSettings{Owner: owner, Tick: getEnvAsDuration("SCHEDULER_TICK", 5*time.Second), LeaseTTL: getEnvAsDuration("LEASE_TTL", 120*time.Second), BeatInterval: getEnvAsDuration("BEAT_INTERVAL", 15*time.Second), JiraGuard: strings.EqualFold(getEnv("SCHEDULER_JIRA_GUARD", "off"), "on")}
}
func Requirements() RequirementSettings {
	return RequirementSettings{Command: getEnv("BASELINE_CMD", "make test"), Timeout: getEnvAsDuration("BASELINE_TIMEOUT", 15*time.Minute), LeaseTTL: getEnvAsDuration("LEASE_TTL", 120*time.Second)}
}
func JiraACField() string { return getEnv("JIRA_AC_FIELD", "") }
