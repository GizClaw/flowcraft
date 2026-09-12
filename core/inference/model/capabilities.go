package model

import (
	"fmt"
	"maps"

	"github.com/GizClaw/flowcraft/core/message"
)

// ModelCapabilities describes optional feature bits and content kinds a model
// can serve. Zero is the conservative declaration: every feature the struct
// omits is treated as unsupported until a provider declares it.
type ModelCapabilities struct {
	// Inputs lists the canonical content part kinds the model accepts as
	// request input (text, image, audio, video, tool calls, ...). Providers
	// that do not declare inputs leave the capability unknown rather than
	// asserted absent; routing falls back to operation support and preflight
	// remains the final arbiter.
	Inputs []message.PartKind `json:"inputs,omitempty"`
	// Outputs lists the canonical content part kinds the model can produce as
	// output. Only the output modalities (text, image, audio, video) are
	// representable; routing prefers targets whose declared outputs cover the
	// request intent and skips declared-incompatible tiers. Empty outputs are
	// undeclared and do not filter routing.
	Outputs []message.PartKind `json:"outputs,omitempty"`
	// Reasoning declares the model's reasoning control capability: whether it
	// has a reasoning channel and whether reasoning can be switched or its
	// effort adjusted, and how the canonical effort levels map onto the
	// model's own wire levels. Empty (ReasoningNone) is the conservative
	// default.
	Reasoning ReasoningCapability `json:"reasoning,omitzero"`
	// HostedWebSearch marks provider-side web_search tool support. It is
	// discovery metadata for hosts; the search configuration itself still
	// rides on GenerateRequest.Extensions as a provider GenerateOptions
	// extension.
	HostedWebSearch bool `json:"hosted_web_search,omitempty"`
	// CustomEmbedDimensions marks embed models whose API accepts the
	// optional output-dimensions parameter on EmbedRequest. Like
	// HostedWebSearch it is discovery metadata: hosts surface the knob from
	// the descriptor, and compilers reject the field for models without
	// it. Drivers with exact-size constraints (for example Qwen's embed
	// whitelist) validate the requested size against their own catalog
	// facts at compile time.
	CustomEmbedDimensions bool `json:"custom_embed_dimensions,omitempty"`
}

// Clone returns a defensive copy of the capabilities: the returned value
// shares no backing array with the receiver.
func (c ModelCapabilities) Clone() ModelCapabilities {
	c.Inputs = append([]message.PartKind(nil), c.Inputs...)
	c.Outputs = append([]message.PartKind(nil), c.Outputs...)
	if c.Reasoning.EffortMap != nil {
		c.Reasoning.EffortMap = maps.Clone(c.Reasoning.EffortMap)
	}
	return c
}

// WithInputs returns a copy of the capabilities with the given input content
// kinds appended. The result shares no backing array with the receiver, so
// calls compose safely.
func (c ModelCapabilities) WithInputs(kinds ...message.PartKind) ModelCapabilities {
	c.Inputs = append(append([]message.PartKind(nil), c.Inputs...), kinds...)
	return c
}

// WithOutputs returns a copy of the capabilities with the given output
// content kinds appended. The result shares no backing array with the
// receiver, so calls compose safely.
func (c ModelCapabilities) WithOutputs(kinds ...message.PartKind) ModelCapabilities {
	c.Outputs = append(append([]message.PartKind(nil), c.Outputs...), kinds...)
	return c
}

// WithReasoning returns a copy of the capabilities with the reasoning control
// capability set.
func (c ModelCapabilities) WithReasoning(kind ReasoningKind) ModelCapabilities {
	c.Reasoning.Kind = kind
	return c
}

// WithReasoningEffortMap returns a copy of the capabilities with the model's
// canonical-to-wire effort map set. The result shares no backing map with
// the receiver or the caller.
func (c ModelCapabilities) WithReasoningEffortMap(
	efforts map[ReasoningEffort]string,
) ModelCapabilities {
	if efforts != nil {
		efforts = maps.Clone(efforts)
	}
	c.Reasoning.EffortMap = efforts
	return c
}

// WithHostedWebSearch returns a copy of the capabilities with hosted web
// search marked supported.
func (c ModelCapabilities) WithHostedWebSearch() ModelCapabilities {
	c.HostedWebSearch = true
	return c
}

// WithCustomEmbedDimensions returns a copy of the capabilities with custom
// embed output dimensions marked supported.
func (c ModelCapabilities) WithCustomEmbedDimensions() ModelCapabilities {
	c.CustomEmbedDimensions = true
	return c
}

func (c ModelCapabilities) Validate() error {
	if err := c.Reasoning.Validate(); err != nil {
		return err
	}
	if err := validatePartKinds(c.Inputs, true); err != nil {
		return err
	}
	return validatePartKinds(c.Outputs, false)
}

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

// validatePartKinds checks one content-kind list: kinds must be valid
// (inputs) or representable output modalities (outputs) and must not
// repeat.
func validatePartKinds(kinds []message.PartKind, inputs bool) error {
	seen := make(map[message.PartKind]struct{}, len(kinds))
	for _, kind := range kinds {
		if inputs {
			if err := kind.Validate(); err != nil {
				return fmt.Errorf("input content kind: %w", err)
			}
		} else if !isOutputModality(kind) {
			return fmt.Errorf(
				"output content kind %q is not a representable output modality",
				kind,
			)
		}
		if _, ok := seen[kind]; ok {
			word := "input"
			if !inputs {
				word = "output"
			}
			return fmt.Errorf("duplicate %s content kind %q", word, kind)
		}
		seen[kind] = struct{}{}
	}
	return nil
}

// isOutputModality reports whether the content kind is a representable output
// modality: the four kinds the generate intent can request.
func isOutputModality(kind message.PartKind) bool {
	switch kind {
	case message.PartText, message.PartImage, message.PartAudio, message.PartVideo:
		return true
	default:
		return false
	}
}
