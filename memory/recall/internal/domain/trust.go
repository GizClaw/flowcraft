package domain

// TrustContext carries read-time visibility constraints.
// Write-path governance (SensitivityPolicy) may stamp facts with
// MetaSensitivity; policy_filter enforces the caller's ceiling at
// recall time.
type TrustContext struct {
	// MaxSensitivity is the highest sensitivity label the caller may
	// see ("public", "internal", "private", "secret"). Empty means no
	// sensitivity ceiling.
	MaxSensitivity string
	// ActorID is the requesting agent. When set, facts written for a
	// different agent (Scope.AgentID) are removed unless the fact is
	// shared (empty agent id).
	ActorID string
	// Scopes limits hits to facts whose Scope matches one of the
	// entries (runtime+user+agent). Empty means no extra scope filter
	// beyond the primary recall scope enforced at materialize.
	Scopes []Scope
}
