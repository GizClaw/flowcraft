package bytedance

import (
	"github.com/GizClaw/flowcraft/sdk/inference"
)

// openGenerate binds the Responses API pipeline for one generate model. The
// compiler is shared by unary and stream shapes; the execution shape flag in
// the wire selects the transport path.
func openGenerate(
	cls *clients,
	spec Spec,
	entry catalogEntry,
	id inference.ModelID,
	profile string,
) (inference.GenerateOperations, error) {
	ark, err := cls.requireArk(profile)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.BindGenerateOperations(
		compileGenerate(cls.endpoint(id.Name), entry),
		transportGenerate(ark),
		decodeGenerate,
		transportGenerateStream(ark),
		decodeGenerateStream,
	)
}