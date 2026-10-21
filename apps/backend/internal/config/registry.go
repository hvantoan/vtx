package config

import (
	"fmt"

	"github.com/vtx/platform/internal/domain"
)

// ValidateRegistry is the fail-closed gate for the VTX-3 registry API (D5).
// It is deliberately separate from Validate(): Validate() guards the Jira
// webhook intake and runs in every binary, while the registry surface only
// exists where cmd/api wires it. Every actor needs its own token; sharing a
// token would erase the audit distinction between principals.
func (c *Config) ValidateRegistry() error {
	var missing []string
	for _, actor := range []domain.Actor{
		domain.ActorScheduler, domain.ActorOrchestrator, domain.ActorOperator, domain.ActorPolicy,
	} {
		if c.RegistryActorTokens[actor] == "" {
			missing = append(missing, actorTokenEnv(actor))
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("registry actor tokens are required: %v", missing)
	}

	// A token must identify exactly one principal, and none of them may be the
	// dashboard or gateway token: those already open other surfaces.
	seen := make(map[string]domain.Actor, len(c.RegistryActorTokens))
	for actor, token := range c.RegistryActorTokens {
		if other, ok := seen[token]; ok {
			return fmt.Errorf("%s and %s must differ", actorTokenEnv(other), actorTokenEnv(actor))
		}
		seen[token] = actor
		if token == c.AuthToken {
			return fmt.Errorf("%s must differ from API_AUTH_TOKEN", actorTokenEnv(actor))
		}
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
	case domain.ActorOperator:
		return "REGISTRY_ACTOR_OPERATOR_TOKEN"
	case domain.ActorPolicy:
		return "REGISTRY_ACTOR_POLICY_TOKEN"
	default:
		return "REGISTRY_ACTOR_" + string(actor) + "_TOKEN"
	}
}
