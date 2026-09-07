package inference

import (
	"encoding/json"
	"maps"

	"github.com/GizClaw/flowcraft/core/message"
)

// CapabilitiesPatch declares capability changes relative to a base model.
// Field presence is the contract: an absent field inherits the base value
// (the built-in catalog entry the declaration overrides); a present field
// replaces that leaf, so narrowing (a shorter inputs list), removal
// (hosted_web_search: false, inputs: []), and reasoning sub-edits are all
// explicit instead of accidental. Applying a patch to a zero base yields the
// conservative declaration, so drivers without a built-in line-up (Azure)
// keep requiring every published capability to be stated.
type CapabilitiesPatch struct {
	// Inputs replaces the base input content kinds when present. An empty
	// list is a declaration of no text-channel inputs, distinct from an
	// absent field that inherits the base list.
	Inputs *[]message.PartKind `json:"inputs,omitempty"`
	// Outputs replaces the base output content kinds when present. Empty
	// means no generate outputs (embed-family declarations).
	Outputs *[]message.PartKind `json:"outputs,omitempty"`
	// Reasoning is a sub-patch: absent inherits the base reasoning control
	// surface wholesale; present overrides only the leaves it names. The
	// legacy `reasoning: "toggle"` string form overrides just the kind.
	Reasoning *ReasoningPatch `json:"reasoning,omitempty"`
	// HostedWebSearch overrides provider-side web search support when
	// present; false explicitly removes the capability from a base that
	// declares it.
	HostedWebSearch *bool `json:"hosted_web_search,omitempty"`
	// CustomEmbedDimensions overrides custom embed output dimension support
	// when present; false explicitly removes the capability from a base
	// that declares it.
	CustomEmbedDimensions *bool `json:"custom_embed_dimensions,omitempty"`
}

// Apply returns the effective capabilities: base with every leaf this patch
// names replaced. A nil patch (the declaration did not include a
// capabilities block at all) inherits base wholesale.
func (p *CapabilitiesPatch) Apply(base ModelCapabilities) ModelCapabilities {
	out := base.Clone()
	if p == nil {
		return out
	}
	if p.Inputs != nil {
		out.Inputs = append([]message.PartKind(nil), (*p.Inputs)...)
	}
	if p.Outputs != nil {
		out.Outputs = append([]message.PartKind(nil), (*p.Outputs)...)
	}
	if p.Reasoning != nil {
		out.Reasoning = p.Reasoning.Apply(base.Reasoning)
	}
	if p.HostedWebSearch != nil {
		out.HostedWebSearch = *p.HostedWebSearch
	}
	if p.CustomEmbedDimensions != nil {
		out.CustomEmbedDimensions = *p.CustomEmbedDimensions
	}
	return out
}

// Validate checks the leaves the patch names: content kinds must be valid
// and unique and a reasoning sub-patch must be coherent. Absent leaves have
// no declaration to check; the merged result is validated after Apply.
func (p *CapabilitiesPatch) Validate() error {
	if p == nil {
		return nil
	}
	if p.Inputs != nil {
		if err := validatePartKinds(*p.Inputs, true); err != nil {
			return err
		}
	}
	if p.Outputs != nil {
		if err := validatePartKinds(*p.Outputs, false); err != nil {
			return err
		}
	}
	if p.Reasoning != nil {
		if err := p.Reasoning.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// ReasoningPatch is the reasoning leaf of a CapabilitiesPatch. Absent
// fields inherit the base reasoning capability; present fields replace
// that leaf only.
type ReasoningPatch struct {
	// Kind replaces the reasoning kind when present. An explicit empty
	// string (ReasoningNone) removes reasoning control from a base that
	// declares it; the legacy `reasoning: ""` string form spells this.
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
