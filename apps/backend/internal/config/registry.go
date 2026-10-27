package config

import (
	"fmt"

	"github.com/vtx/platform/internal/domain"
)

// ValidateRegistry is the fail-closed gate for the VTX-3 registry API (D5).
// It is deliberately separate from Validate(): Validate() guards the Jira
// webhook intake and runs in every binary, while the registry surface only
// exists where cmd/api wires it. Every machine actor needs its own token;
// sharing a token would erase the audit distinction between principals.
// The operator is not a machine actor: it authenticates with an app session.
func (c *Config) ValidateRegistry() error {
	var missing []string
	for _, actor := range []domain.Actor{
		domain.ActorScheduler, domain.ActorOrchestrator, domain.ActorPolicy,
	} {
		if c.RegistryActorTokens[actor] == "" {
			missing = append(missing, actorTokenEnv(actor))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("registry actor tokens are required: %v", missing)
	}

	// A token must identify exactly one principal, and none of them may be the
	// gateway token: it already opens the webhook surface.
	seen := make(map[string]domain.Actor, len(c.RegistryActorTokens))
	for actor, token := range c.RegistryActorTokens {
		if other, ok := seen[token]; ok {
			return fmt.Errorf("%s and %s must differ", actorTokenEnv(other), actorTokenEnv(actor))
		}
		seen[token] = actor
		if token == c.GatewayInternalAuthToken {
			return fmt.Errorf("%s must differ from GATEWAY_INTERNAL_AUTH_TOKEN", actorTokenEnv(actor))
		}
	}
	return nil
}

func actorTokenEnv(actor domain.Actor) string {
	switch actor {
	case domain.ActorScheduler:
		return "REGISTRY_ACTOR_SCHEDULER_TOKEN"
	case domain.ActorOrchestrator:
		return "REGISTRY_ACTOR_ORCHESTRATOR_TOKEN"
	case domain.ActorPolicy:
		return "REGISTRY_ACTOR_POLICY_TOKEN"
	default:
		return "REGISTRY_ACTOR_" + string(actor) + "_TOKEN"
	}
}
