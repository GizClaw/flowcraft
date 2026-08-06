package anthropic

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// openGenerate binds the generate pipeline for one catalog model through
// the kernel: the anthropic provider is the kernel's first consumer, and
// sibling Claude platforms (Bedrock, Vertex) bind the same drivers with
// their own clients and capability declarations.
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	return KernelGenerate(cls.api, id.Name, Capabilities{
		Vision:           entry.vision,
		Reasoning:        entry.reasoning,
		ReasoningLevels:  entry.reasoningLevels,
		ReasoningDisable: entry.reasoningDisable,
	})
}
