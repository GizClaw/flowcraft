package bytedance

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
)

// openGenerate binds the Responses API pipeline for one generate model. The
// same compiler serves both execution shapes — it states the shape on the
// request — and each shape has its own transport.
func openGenerate(
	cls *clients,
	spec Spec,
	entry catalogEntry,
	id model.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	ark, err := cls.requireArk(profile)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	// Ark produces reasoning traces but consumes none, so the stamp keeps
	// them attributable when a conversation later moves to a target that
	// would replay one. The scope is the deployment's declared token, or the
	// address that produced the trace.
	scope := inference.ReasoningScope(
		entry.reasoningScopeDeclared, id.Provider, id.Name, profile)
	entry.reasoningScope = scope
	return inference.BindGenerateOperations(
		compileGenerate(cls.endpoint(id.Name), entry),
		transportGenerate(ark, cls.arkRequestOptions),
		inference.WithReasoningSource(decodeGenerate, scope),
		transportGenerateStream(ark, cls.arkRequestOptions),
		inference.WithReasoningDeltaSource(decodeGenerateStream, scope),
	)
}
