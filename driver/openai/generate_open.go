package openai

import (
	"github.com/GizClaw/flowcraft/core/inference"
)

// openGenerate binds the generate pipeline for one catalog model through
// the kernel: the openai provider is the kernel's first consumer, and
// sibling providers (Azure OpenAI) bind the same drivers with their own
// clients and capability declarations.
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	if entry.api == apiChat {
		return KernelChatGenerate(cls.api, id.Name, Capabilities{
			Vision:    entry.vision,
			Reasoning: entry.reasoning,
		})
	}
	return KernelGenerate(cls.api, id.Name, Capabilities{
		Vision:    entry.vision,
		Reasoning: entry.reasoning,
		WebSearch: entry.webSearch,
	})
}
