package bytedance

// dialect is one provider instance's wire policy: what the deployment decided
// that is not about a single model. It is derived from the Spec once per
// provider and shared by every model's compiler.
type dialect struct {
	// scopeDeclared replaces the derived reasoning verification scope when
	// the operator has verified that several models or credentials accept
	// each other's traces. Ark emits traces but consumes none today, so this
	// only makes a stored trace attributable.
	scopeDeclared string
}

// dialect derives the provider-wide wire policy from the spec.
func (s Spec) dialect() dialect {
	return dialect{scopeDeclared: s.ReasoningScope}
}
