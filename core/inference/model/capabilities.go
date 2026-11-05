package model

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/message"
)

// ModelCapabilities describes optional feature bits and content kinds a model
// can serve. Zero is the conservative declaration: every feature the struct
// omits is treated as unsupported until a provider declares it.
type ModelCapabilities struct {
	// Inputs lists the canonical content part kinds the model accepts as
	// request input (text, image, audio, video, tool calls, ...). Providers
	// that do not declare inputs leave the capability unknown rather than
	// asserted absent: an empty list filters nothing, while a declared list
	// rejects a request carrying an undeclared part before any driver opens,
	// and the compiler remains the final arbiter for everything the
	// declaration does not cover (the baseline kinds, and structural parts
	// whose role rules are per surface).
	Inputs []message.PartKind `json:"inputs,omitempty"`
	// Outputs lists the canonical content part kinds the model can produce as
	// output. Only the output modalities (text, image, audio, video) are
	// representable; routing prefers targets whose declared outputs cover the
	// request intent and skips declared-incompatible tiers. Empty outputs are
	// undeclared and do not filter routing.
	Outputs []message.PartKind `json:"outputs,omitempty"`
	// ToolCalling marks function-tool support. Tool definitions are
	// canonical data (name, description, JSON Schema), so a request carries
	// them directly; this bit lets a driver reject tools for a model that has
	// no tool channel before any provider I/O, the same way
	// CustomEmbedDimensions rejects its knob. Zero means undeclared, which
	// filters nothing.
	ToolCalling bool `json:"tool_calling,omitempty"`
	// HostedWebSearch marks provider-side web_search tool support. It is
	// discovery metadata for hosts; the search configuration itself still
	// rides on GenerateRequest.Extensions as a provider GenerateOptions
	// extension.
	HostedWebSearch bool `json:"hosted_web_search,omitempty"`
}

// Clone returns a defensive copy of the capabilities: the returned value
// shares no backing array with the receiver.
func (c ModelCapabilities) Clone() ModelCapabilities {
	c.Inputs = append([]message.PartKind(nil), c.Inputs...)
	c.Outputs = append([]message.PartKind(nil), c.Outputs...)
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

// WithToolCalling returns a copy of the capabilities with function-tool
// support marked available.
func (c ModelCapabilities) WithToolCalling() ModelCapabilities {
	c.ToolCalling = true
	return c
}

// WithHostedWebSearch returns a copy of the capabilities with hosted web
// search marked supported.
func (c ModelCapabilities) WithHostedWebSearch() ModelCapabilities {
	c.HostedWebSearch = true
	return c
}

func (c ModelCapabilities) Validate() error {
	if err := validatePartKinds(c.Inputs, true); err != nil {
		return err
	}
	return validatePartKinds(c.Outputs, false)
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
