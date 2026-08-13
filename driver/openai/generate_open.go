package openai

import (
	"github.com/GizClaw/flowcraft/core/inference"
)

// openGenerate binds the generate pipeline (unary + stream) for one
// catalog model. The provider owns its kernel: the compile/transport/decode
// stages live in this package, and openGenerate wires them for the model's
// API mode (Responses by default, Chat Completions when spec.api is chat).
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	if entry.api == apiChat {
		return inference.BindGenerateOperations(
			compileGenerate(id.Name, entry),
			transportChatGenerate(cls.api),
			decodeGenerate,
			transportChatGenerateStream(cls.api),
			decodeChatGenerateStream,
		)
	}
	return inference.BindGenerateOperations(
		compileGenerate(id.Name, entry),
		transportGenerate(cls.api),
		decodeGenerate,
		transportGenerateStream(cls.api),
		decodeGenerateStream,
	)
}
