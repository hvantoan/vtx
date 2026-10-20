package config_test

import (
	"strings"
	"testing"

	"github.com/vtx/platform/internal/config"
	"github.com/vtx/platform/internal/domain"
)

// TestValidateRegistryFailsClosed: D5 — every actor needs its own token, and no
// actor token may reuse the dashboard or gateway token.
func TestValidateRegistryFailsClosed(t *testing.T) {
	base := func() *config.Config {
		return &config.Config{
			AuthToken:                "public",
			GatewayInternalAuthToken: "gateway",
			RegistryActorTokens: map[domain.Actor]string{
				domain.ActorScheduler:    "tok-s",
				domain.ActorOrchestrator: "tok-o",
				domain.ActorOperator:     "tok-h",
				domain.ActorPolicy:       "tok-p",
			},
		}
	}

	if err := base().ValidateRegistry(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*config.Config)
		want   string
	}{
		{"missing scheduler", func(c *config.Config) { c.RegistryActorTokens[domain.ActorScheduler] = "" }, "REGISTRY_ACTOR_SCHEDULER_TOKEN"},
		{"missing policy", func(c *config.Config) { delete(c.RegistryActorTokens, domain.ActorPolicy) }, "REGISTRY_ACTOR_POLICY_TOKEN"},
		{"duplicate tokens", func(c *config.Config) { c.RegistryActorTokens[domain.ActorPolicy] = "tok-s" }, "must differ"},
		{"reuses dashboard token", func(c *config.Config) { c.RegistryActorTokens[domain.ActorOperator] = "public" }, "API_AUTH_TOKEN"},
		{"reuses gateway token", func(c *config.Config) { c.RegistryActorTokens[domain.ActorOrchestrator] = "gateway" }, "GATEWAY_INTERNAL_AUTH_TOKEN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base()
			tc.mutate(cfg)
			err := cfg.ValidateRegistry()
			if err == nil {
				t.Fatal("config accepted, want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}
