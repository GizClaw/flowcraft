package minimax

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdkx/inference/anthropic"
)

// openGenerate binds the chat driver for one catalog model through the
// anthropic kernel: MiniMax serves the Messages protocol with signed
// thinking blocks, so the kernel supplies the compiler, transport, and
// decoders — this package supplies the client, the model name, and the
// capability declaration. Every MiniMax model speaks the binary-thinking
// dialect: the reasoning intent compiles to thinking: {type: "adaptive"}
// because the endpoint has no effort levels.
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	return anthropic.KernelGenerate(cls.api, id.Name, anthropic.Capabilities{
		Vision:           entry.vision,
		Reasoning:        entry.reasoning,
		ReasoningDisable: entry.reasoningDisable,
		ReasoningControl: anthropic.ReasoningControlAdaptive,
	})
}
