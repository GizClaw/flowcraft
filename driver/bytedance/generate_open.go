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
	return inference.BindGenerateOperations(
		compileGenerate(cls.endpoint(id.Name), entry),
		transportGenerate(ark, cls.arkRequestOptions),
		decodeGenerate,
		transportGenerateStream(ark, cls.arkRequestOptions),
		decodeGenerateStream,
	)
}
