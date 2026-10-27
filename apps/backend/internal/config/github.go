package config

import (
	"fmt"
	"net/url"

	"github.com/vtx/platform/internal/domain"
)

// ValidateGitHub is called only when the draft-PR integration is wired.
func (c *Config) ValidateGitHub() error {
	if c.GitHubToken == "" {
		return fmt.Errorf("GITHUB_TOKEN is required")
	}
	u, err := url.Parse(c.GitHubAPIBaseURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("GITHUB_API_BASE_URL must be an https URL")
	}
	if c.GitHubToken == c.GatewayInternalAuthToken {
		return fmt.Errorf("GITHUB_TOKEN must differ from GATEWAY_INTERNAL_AUTH_TOKEN")
	}
	for _, actor := range []domain.Actor{domain.ActorScheduler, domain.ActorOrchestrator, domain.ActorPolicy} {
		if c.GitHubToken == c.RegistryActorTokens[actor] {
			return fmt.Errorf("GITHUB_TOKEN must differ from %s", actorTokenEnv(actor))
		}
	}
	return nil
}
