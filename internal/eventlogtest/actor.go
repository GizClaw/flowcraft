package eventlogtest

import (
	"testing"

	"github.com/GizClaw/flowcraft/internal/policy"
)

// TestUser returns a test user actor in the given realm.
func TestUser(t testing.TB, id, realmID string) policy.Actor {
	return policy.Actor{
		Type:    policy.ActorUser,
		ID:      id,
		RealmID: realmID,
	}
}

// TestSuperAdmin returns a super admin actor.
func TestSuperAdmin(t testing.TB) policy.Actor {
	return policy.Actor{
		Type:  policy.ActorUser,
		ID:    "admin",
		Super: true,
	}
}

// TestAgent returns a test agent actor.
func TestAgent(t testing.TB, id, realmID, runtimeID string) policy.Actor {
	return policy.Actor{
		Type:      policy.ActorAgent,
		ID:        id,
		RealmID:   realmID,
		RuntimeID: runtimeID,
	}
}

// TestSystem returns a system actor for the given component.
func TestSystem(t testing.TB, componentName string) policy.Actor {
	return policy.SystemActor(componentName)
}
