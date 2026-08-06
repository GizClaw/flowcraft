package deepseek

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// openGenerate binds the chat driver (chat completions, unary + stream)
// for one catalog model.
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id inference.ModelID,
	_ string,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileGenerate(id.Name, entry),
		transportGenerate(cls.api),
		decodeGenerate,
		transportGenerateStream(cls.api),
		decodeGenerateStream,
	)
}
