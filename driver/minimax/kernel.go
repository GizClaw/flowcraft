package minimax

import (
	"github.com/GizClaw/flowcraft/core/inference"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

// bindGenerate binds the chat driver (Messages API, unary + stream) for one
// catalog model: the kernel supplies the compiler, transports, and
// decoders; this package supplies the client, model name, and capability
// declaration. Every MiniMax model speaks the binary-thinking dialect, so
// the reasoning intent always compiles to thinking: {type: "adaptive"}.
func bindGenerate(
	client anthropicgo.Client,
	model string,
	vision, reasoning, reasoningDisable bool,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileGenerate(model, catalogEntry{
			kind:             kindGenerate,
			vision:           vision,
			reasoning:        reasoning,
			reasoningDisable: reasoningDisable,
		}),
		transportGenerate(client),
		decodeGenerate,
		transportGenerateStream(client),
		decodeGenerateStream,
	)
}
