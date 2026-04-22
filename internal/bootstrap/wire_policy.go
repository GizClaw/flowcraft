package bootstrap

import "github.com/GizClaw/flowcraft/internal/policy"

// WirePolicy constructs the production authorization policy. Pass a real
// RealmStore (typically the SQLite store) so the runtime/agent rules can
// resolve realm membership.
func WirePolicy(realmStore policy.RealmStore) policy.Policy {
	return policy.NewDefaultPolicy(realmStore)
}
