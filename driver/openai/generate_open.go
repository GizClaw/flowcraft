package openai

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// openGenerate binds the generate pipeline (unary + stream) for one
// catalog model. The provider owns its kernel: the compile/transport/decode
// stages live in this package, and openGenerate wires them for the model's
// API mode (Responses by default, Chat Completions when spec.api is chat).
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	// One scope for the whole attempt: the decoder stamps it on the traces
	// this model produces, and the compiler requires it before replaying one,
	// so a trace never crosses a deployment, a model, or an account unless
	// the deployment declared a shared scope.
	scope := inference.ReasoningScope(
		entry.dialect.reasoningScopeDeclared, id.Provider, id.Name, profile)
	entry.reasoningScope = scope
	if entry.dialect.api == apiChat {
		return inference.BindGenerateOperations(
			compileChat(id.Name, entry),
			transportChatGenerate(cls.api),
			inference.WithReasoningSource(decodeGenerate, scope),
			transportChatGenerateStream(cls.api),
			inference.WithReasoningDeltaSource(decodeChatGenerateStream, scope),
		)
	}
	return inference.BindGenerateOperations(
		compileResponses(id.Name, entry),
		transportGenerate(cls.api),
		inference.WithReasoningSource(decodeGenerate, scope),
		transportGenerateStream(cls.api),
		inference.WithReasoningDeltaSource(decodeGenerateStream, scope),
	)
}
