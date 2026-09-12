package anthropic

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// openGenerate binds the generate pipeline (Messages API, unary + stream)
// for one catalog model. The provider owns its kernel: the compile,
// transport, and decode stages live in this package, and openGenerate
// wires them with the model's capability declaration. Anthropic serves the
// effort dialect, so the reasoning intent compiles to output_config.effort.
func openGenerate(
	cls *clients,
	entry catalogEntry,
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	// One scope for the whole attempt: the decoder stamps it on the thinking
	// traces this model produces, and the compiler requires it before
	// replaying a stored one. Anthropic verifies a thinking signature per
	// model, so the derived default refuses to cross a model or an account
	// unless the deployment declared a shared scope.
	scope := inference.ReasoningScope(
		entry.reasoningScopeDeclared, id.Provider, id.Name, profile)
	entry.reasoningScope = scope
	return inference.BindGenerateOperations(
		compileGenerate(id.Name, entry),
		transportGenerate(cls.api),
		inference.WithReasoningSource(decodeGenerate, scope),
		transportGenerateStream(cls.api),
		inference.WithReasoningDeltaSource(decodeGenerateStream, scope),
	)
}
