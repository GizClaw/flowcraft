package model

import (
	"encoding/json"
	"fmt"
	"maps"
	"unicode"
)

// ReasoningKind declares a model's reasoning control capability. Zero is the
// conservative declaration: a model without a declared reasoning capability
// has no reasoning channel or reasoning controls.
//
// Kind is a promise about the provider surface that publishes it, not just
// about the physical model: a model published as ReasoningToggle must compile
// reasoning_enabled=false successfully on that provider instance, and one
// published as ReasoningAlways may reject it. Providers whose wire cannot
// express "off" for a model on a given surface (for example Chat Completions
// for models whose Responses surface accepts reasoning.effort=none) must
// publish ReasoningAlways on that surface; driver conformance suites verify
// the promise for every published entry.
type ReasoningKind string

const (
	ReasoningNone   ReasoningKind = ""
	ReasoningAlways ReasoningKind = "always"
	ReasoningToggle ReasoningKind = "toggle"
)

func (k ReasoningKind) Validate() error {
	switch k {
	case ReasoningNone, ReasoningAlways, ReasoningToggle:
		return nil
	default:
		return fmt.Errorf("unknown reasoning kind %q", k)
	}
}

// ReasoningEffort is the request-side "how hard should the model think"
// knob. It is a wire-level string enum, but it is an inference concept
// (not a message part, not a tool DTO) so it lives here rather than in
// [github.com/GizClaw/flowcraft/core/message]. The five constants are the
// portable ordinal ladder every request may name; each model declares how
// the canonical levels map onto its own wire levels in
// ModelCapabilities.Reasoning.EffortMap.
type ReasoningEffort string

const (
	ReasoningMinimal ReasoningEffort = "minimal"
	ReasoningLow     ReasoningEffort = "low"
	ReasoningMedium  ReasoningEffort = "medium"
	ReasoningHigh    ReasoningEffort = "high"
	ReasoningXHigh   ReasoningEffort = "xhigh"
)

// ReasoningCapability declares a model's reasoning control surface. Kind
// keeps the switch semantics (none / always / toggle): none has no
// reasoning channel, always cannot be turned off, and toggle promises that
// reasoning_enabled=false compiles on the publishing provider surface.
// EffortMap declares exactly how the five canonical ReasoningEffort levels
// map onto this model's own wire levels (for example kimi-k3 maps xhigh to
// "max"). A non-nil EffortMap must cover all five canonical levels; an
// empty map on a reasoning model means binary thinking with no depth dial.
type ReasoningCapability struct {
	Kind ReasoningKind `json:"kind,omitempty"`
	// EffortMap maps each canonical ReasoningEffort onto the model's wire
	// level token. Values may be model-specific (for example "max" on
	// kimi-k3); a value different from its key means the canonical level
	// folds onto another level and compilers report the drop.
	EffortMap map[ReasoningEffort]string `json:"effort_map,omitempty"`
}

// IsZero reports whether the capability is the zero declaration.
func (r ReasoningCapability) IsZero() bool {
	return r.Kind == "" && len(r.EffortMap) == 0
}

// Validate checks the capability for coherence: the kind must be valid, a
// none kind cannot carry an effort map, and a non-empty map must cover all
// five canonical levels with well-formed wire tokens.
func (r ReasoningCapability) Validate() error {
	if err := r.Kind.Validate(); err != nil {
		return err
	}
	if r.Kind == ReasoningNone {
		if len(r.EffortMap) == 0 {
			return nil
		}
		return fmt.Errorf("reasoning effort map requires a reasoning capability")
	}
	return validateReasoningEffortMap(r.EffortMap)
}

// validateReasoningEffortMap checks a canonical-to-wire effort map: a
// non-empty map must cover all five canonical levels with well-formed wire
// tokens and contain no foreign keys. An empty map is the no-dial
// declaration.
func validateReasoningEffortMap(efforts map[ReasoningEffort]string) error {
	if len(efforts) == 0 {
		return nil
	}
	for _, effort := range []ReasoningEffort{
		ReasoningMinimal,
		ReasoningLow,
		ReasoningMedium,
		ReasoningHigh,
		ReasoningXHigh,
	} {
		mode, ok := efforts[effort]
		if !ok {
			return fmt.Errorf(
				"reasoning effort map misses canonical level %q",
				effort,
			)
		}
		if err := validateEffortToken(mode); err != nil {
			return fmt.Errorf(
				"reasoning effort map %q: %w",
				effort,
				err,
			)
		}
	}
	for effort := range efforts {
		switch effort {
		case ReasoningMinimal, ReasoningLow, ReasoningMedium,
			ReasoningHigh, ReasoningXHigh:
		default:
			return fmt.Errorf(
				"reasoning effort map has non-canonical key %q",
				effort,
			)
		}
	}
	return nil
}

// ResolveEffort returns the wire level this model declares for a canonical
// effort. The boolean reports whether the map declares the level at all;
// validated maps always do.
func (r ReasoningCapability) ResolveEffort(
	effort ReasoningEffort,
) (string, bool) {
	mode, ok := r.EffortMap[effort]
	return mode, ok
}

// UnmarshalJSON accepts both the legacy string form ("toggle") and the
// object form ({"kind":"toggle","effort_map":{...}}) so existing
// deployment specs keep decoding.
func (r *ReasoningCapability) UnmarshalJSON(data []byte) error {
	var kind ReasoningKind
	if err := json.Unmarshal(data, &kind); err == nil {
		r.Kind = kind
		r.EffortMap = nil
		return nil
	}
	type alias ReasoningCapability
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*r = ReasoningCapability(decoded)
	return nil
}

// MarshalJSON keeps the legacy string form when no effort map is declared,
// so a deployment that decoded `reasoning: "toggle"` re-serializes to the
// same string instead of leaking the new object shape. Capabilities with an
// effort map marshal as the object form.
func (r ReasoningCapability) MarshalJSON() ([]byte, error) {
	if r.IsZero() {
		return []byte(`{}`), nil
	}
	if len(r.EffortMap) == 0 {
		return json.Marshal(r.Kind)
	}
	type alias ReasoningCapability
	return json.Marshal(alias(r))
}

// ReasoningPatch is the reasoning leaf of a CapabilitiesPatch. Absent
// fields inherit the base reasoning capability; present fields replace
// that leaf only.
type ReasoningPatch struct {
	// Kind replaces the reasoning kind when present. An explicit empty
	// string (ReasoningNone) removes reasoning control from a base that
	// declares it, dropping the inherited effort map with it (a none kind
	// cannot carry a dial); the legacy `reasoning: ""` string form spells
	// this.
	Kind *ReasoningKind `json:"kind,omitempty"`
	// EffortMap replaces the canonical-to-wire effort map when present. An
	// empty map clears a base dial, leaving binary thinking with no depth
	// control.
	EffortMap *map[ReasoningEffort]string `json:"effort_map,omitempty"`
}

// Apply returns the base reasoning capability with every named leaf
// replaced.
func (p *ReasoningPatch) Apply(base ReasoningCapability) ReasoningCapability {
	out := base
	if base.EffortMap != nil {
		out.EffortMap = maps.Clone(base.EffortMap)
	}
	if p == nil {
		return out
	}
	if p.Kind != nil {
		out.Kind = *p.Kind
	}
	if p.EffortMap != nil {
		out.EffortMap = maps.Clone(*p.EffortMap)
	}
	if out.Kind == ReasoningNone && p.EffortMap == nil {
		// None cannot carry a dial: removing the reasoning capability with
		// the legacy empty-string kind must also drop any inherited effort
		// map, or the merged capability fails validation downstream.
		out.EffortMap = nil
	}
	return out
}

// Validate checks the named leaves: a named kind must be valid and a named
// effort map, when non-empty, must cover all five canonical levels with
// well-formed wire tokens.
func (p *ReasoningPatch) Validate() error {
	if p == nil {
		return nil
	}
	if p.Kind != nil {
		if err := p.Kind.Validate(); err != nil {
			return err
		}
		if *p.Kind == ReasoningNone &&
			p.EffortMap != nil && len(*p.EffortMap) > 0 {
			return fmt.Errorf(
				"reasoning kind none cannot declare an effort map",
			)
		}
	}
	if p.EffortMap != nil {
		return validateReasoningEffortMap(*p.EffortMap)
	}
	return nil
}

// UnmarshalJSON accepts both the legacy string form ("toggle", meaning
// "override the kind only") and the object form so existing deployment
// specs keep decoding and existing redeclarations keep the base effort map
// unless the object names one.
func (p *ReasoningPatch) UnmarshalJSON(data []byte) error {
	var kind ReasoningKind
	if err := json.Unmarshal(data, &kind); err == nil {
		*p = ReasoningPatch{Kind: &kind}
		return nil
	}
	type alias ReasoningPatch
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*p = ReasoningPatch(decoded)
	return nil
}

// MarshalJSON keeps the legacy string form when the patch names only a
// kind, so a deployment that decoded `reasoning: "toggle"` re-serializes to
// the same string instead of leaking the object shape.
func (p ReasoningPatch) MarshalJSON() ([]byte, error) {
	if p.Kind != nil && p.EffortMap == nil {
		return json.Marshal(*p.Kind)
	}
	type alias ReasoningPatch
	return json.Marshal(alias(p))
}

// validateEffortToken checks that a wire-level token is well-formed:
// non-empty, at most 64 characters, and free of whitespace and control
// characters.
func validateEffortToken(token string) error {
	if token == "" {
		return fmt.Errorf("reasoning level must not be empty")
	}
	if len(token) > 64 {
		return fmt.Errorf("reasoning level %q exceeds 64 characters", token)
	}
	for _, r := range token {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf(
				"reasoning level %q contains whitespace or control characters",
				token,
			)
		}
	}
	return nil
}
