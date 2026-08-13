package azure

import (
	"github.com/GizClaw/flowcraft/core/inference"
)

// openGenerate binds the Responses generate pipeline (unary + stream) for
// one deployment through the self-hosted kernel: this package owns the
// compiler, transports, and decoders, and supplies its own Azure client,
// the deployment name, and the capability declaration.
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
