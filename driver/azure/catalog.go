package azure

import (
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// catalogEntry is one deployment's compile-time capability declaration.
// Azure routes by deployment name, so the spec's models list is the whole
// catalog: the factory maps each declared deployment onto an entry, and the
// compiler rejects every channel the entry omits. capabilities is the single
// capability fact source; dimensions and effortNone are control capabilities
// that no capability kind expresses and stay separate flags.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	dimensions   bool
	// effortNone marks generate deployments whose reasoning.effort accepts
	// "none" to disable reasoning; without it a disable request rejects.
	effortNone bool
	// requestMetadataEnvelope is the provider-level lowering policy for
	// canonical GenerateRequest.RequestMetadata ("" disables forwarding).
	requestMetadataEnvelope string
}

// entryFor lowers one declared deployment into a compiler entry.
func entryFor(model ModelSpec) catalogEntry {
	return catalogEntry{
		kind:         modelKind(model.Kind),
		capabilities: model.Capabilities,
		dimensions:   model.Dimensions,
		effortNone:   model.EffortNone,
	}
}

// validate enforces the family contract: the compiler bound by kind can only
// serve the output modalities it produces, so kind and capabilities cannot
// drift.
func (e catalogEntry) validate() error {
	if err := e.capabilities.Validate(); err != nil {
		return err
	}
	switch e.kind {
	case kindGenerate:
		if !slices.Contains(e.capabilities.Outputs, message.PartText) {
			return fmt.Errorf("generate family must declare text output")
		}
	case kindImage:
		if !slices.Contains(e.capabilities.Outputs, message.PartImage) {
			return fmt.Errorf("image family must declare image output")
		}
	case kindTTS:
		if !slices.Contains(e.capabilities.Outputs, message.PartAudio) {
			return fmt.Errorf("tts family must declare audio output")
		}
	case kindEmbed:
		if len(e.capabilities.Outputs) != 0 {
			return fmt.Errorf("embed family declares no generate output")
		}
	}
	return nil
}
