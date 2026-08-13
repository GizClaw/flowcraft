package anthropic

import (
	"github.com/GizClaw/flowcraft/core/inference"
)

// openGenerate binds the generate pipeline (Messages API, unary + stream)
// for one catalog model. The provider owns its kernel: the compile,
// transport, and decode stages live in this package, and openGenerate
// wires them with the model's capability declaration. Anthropic serves the
// effort dialect, so the reasoning intent compiles to output_config.effort.
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
