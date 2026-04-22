package policy

// rules_admin.go centralises rules that gate ADMIN-only operations:
//   * R-ADMIN-1: subscribing without a partition filter (cross-realm fan-out)
//     requires super admin; enforced in DefaultPolicy.AllowSubscribe.
//   * R-ADMIN-2: ReadAll (no partition filter) follows the same gate.
//
// Future admin rules (e.g. realm-scoped admin overrides, super-only event
// types) belong here rather than in the per-partition file so reviewers can
// audit privileged paths in one place.

// IsAdminOnlyEventType returns true when the event type is restricted to
// admin actors regardless of partition. Reserved for future use; currently
// every admin gate is expressed by `actor.IsSuperAdmin()` checks in the
// per-partition rules.
func IsAdminOnlyEventType(t string) bool {
	switch t {
	case "audit.action.failed", "audit.action.performed":
		// audit.* may be appended by system or super admin; subscription is
		// already gated by R-ACTOR-1 because audit lives on the actor partition.
		return true
	}
	return false
}
