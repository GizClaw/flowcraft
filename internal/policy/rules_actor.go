package policy

import "context"

// rules_actor.go owns R-ACTOR-* rules: the audit-only "actor:" partition that
// records what every actor did. By convention only system components or super
// admins can append here, and only super admins can subscribe.

// allowAppendActor enforces R-ACTOR-1: writes to the actor: partition are
// limited to system actors (for audit pipelines) and super admins.
func (p *DefaultPolicy) allowAppendActor(_ context.Context, actor Actor, _ EnvelopeDraft) (Decision, error) {
	if actor.IsSuperAdmin() || actor.Type == ActorSystem {
		return Allow, nil
	}
	return Deny("R-ACTOR-1: actor partition write requires super admin or system"), nil
}
