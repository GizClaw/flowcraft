package anthropic

// dialect is one provider instance's wire policy: the endpoint extensions the
// deployment declared on top of the Messages protocol. It is derived from the
// Spec once per provider and shared by every model's compiler, so the
// compiler reads decisions rather than reaching back into the configuration.
type dialect struct {
	// videoInput makes video content blocks legal on this endpoint: the
	// protocol has no video block, so a compatible endpoint has to state it.
	videoInput bool
	// scopeDeclared replaces the derived reasoning verification scope when
	// the operator has verified that several models or credentials accept
	// each other's signed traces.
	scopeDeclared string
}

// dialect derives the provider-wide wire policy from the spec.
func (s Spec) dialect() dialect {
	return dialect{
		videoInput:    s.Wire.VideoInput,
		scopeDeclared: s.Wire.ReasoningScope,
	}
}
