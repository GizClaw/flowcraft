package deepseek

import (
	"github.com/GizClaw/flowcraft/core/inference"
)

// openGenerate binds the generate pipeline for one catalog model through
// the selected surface: Chat Completions by default, Responses when the
// provider Spec selects `api: responses`.
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	if entry.api == apiResponses {
		return inference.BindGenerateOperations(
			compileResponsesGenerate(id.Name, entry),
			transportResponsesGenerate(cls.api),
			decodeGenerate,
			transportResponsesGenerateStream(cls.api),
			decodeGenerateStream,
		)
	}
	return inference.BindGenerateOperations(
		compileChatGenerate(id.Name, entry),
		transportChatGenerate(cls.api),
		decodeGenerate,
		transportChatGenerateStream(cls.api),
		decodeGenerateStream,
	)
}
