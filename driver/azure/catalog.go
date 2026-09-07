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
// capability fact source, including custom embed output dimensions.
// Reasoning off needs no separate flag: Azure OpenAI expresses it as
// reasoning.effort="none", so a deployment published as ReasoningToggle has
// an off route by construction.
type catalogEntry struct {
	kind         modelKind
	capabilities inference.ModelCapabilities
	// requestMetadataEnvelope is the provider-level lowering policy for
	// canonical GenerateRequest.RequestMetadata ("" disables forwarding).
	requestMetadataEnvelope string
}

// entryFor lowers one declared deployment into a compiler entry. Azure has
// no built-in catalog, so capability leaves are applied to the conservative
// zero base: every published capability must be stated.
func entryFor(model ModelSpec) catalogEntry {
	return catalogEntry{
		kind:         modelKind(model.Kind),
		capabilities: model.Capabilities.Apply(inference.ModelCapabilities{}),
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

// Catalog returns the built-in model catalog under provider. Azure routes by
// deployment name and has no built-in lineup: every model is declared in the
// deployment spec, so Catalog always reports an empty catalog.
func Catalog(provider string) ([]inference.ModelDescriptor, error) {
	if provider == "" {
		return nil, fmt.Errorf("catalog: provider is required")
	}
	return []inference.ModelDescriptor{}, nil
}
