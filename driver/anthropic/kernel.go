package anthropic

import (
	"github.com/GizClaw/flowcraft/core/inference"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

// ReasoningControl names the wire shape one Messages-compatible platform
// uses for the reasoning intent.
type ReasoningControl int

const (
	// ReasoningControlEffort emits output_config.effort.
	ReasoningControlEffort ReasoningControl = iota
	// ReasoningControlAdaptive emits thinking: {type: "adaptive"}.
	ReasoningControlAdaptive
)

// Capabilities declares what one externally managed Claude deployment
// accepts.
type Capabilities struct {
	Vision           bool
	Reasoning        bool
	ReasoningLevels  bool
	ReasoningDisable bool
	ReasoningControl ReasoningControl
}

// KernelGenerate binds the chat driver (Messages API, unary + stream) for
// one externally managed Claude deployment.
func KernelGenerate(
	client anthropicgo.Client,
	model string,
	caps Capabilities,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileGenerate(model, catalogEntry{
			vision:           caps.Vision,
			reasoning:        caps.Reasoning,
			reasoningLevels:  caps.ReasoningLevels,
			reasoningDisable: caps.ReasoningDisable,
		}),
		transportGenerate(client, caps.ReasoningControl),
		decodeGenerate,
		transportGenerateStream(client, caps.ReasoningControl),
		decodeGenerateStream,
	)
}
