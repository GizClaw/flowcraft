package openai

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// openGenerate binds the generate pipeline (unary + stream) for one
// declared model. The provider owns its kernel: the compile/transport/decode
// stages live in this package, and openGenerate wires them for the model's
// API mode (Responses by default, Chat Completions when spec.api is chat).
func openGenerate(
	cls *clients,
	declared ModelSpec,
	wire dialect,
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	// One scope for the whole attempt: the decoder stamps it on the traces
	// this model produces, and the compiler requires it before replaying one,
	// so a trace never crosses a deployment, a model, or an account unless
	// the deployment declared a shared scope.
	scope := inference.ReasoningScope(
		wire.reasoning.scopeDeclared, id.Provider, id.Name, profile)
	if wire.surface.api == apiChat {
		return inference.BindGenerateOperations(
			compileChat(id.Name, declared, wire, scope),
			transportChatGenerate(cls.api),
			inference.WithReasoningSource(decodeGenerate, scope),
			transportChatGenerateStream(cls.api),
			inference.WithReasoningDeltaSource(decodeChatGenerateStream, scope),
		)
	}
	return inference.BindGenerateOperations(
		compileResponses(id.Name, declared, wire, scope),
		transportGenerate(cls.api),
		inference.WithReasoningSource(decodeGenerate, scope),
		transportGenerateStream(cls.api),
		inference.WithReasoningDeltaSource(decodeGenerateStream, scope),
	)
}
