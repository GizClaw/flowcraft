package inferencetest

import (
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// ReasoningOffProbe returns the canonical unary GenerateRequest that driver
// conformance suites use to verify the reasoning "toggle" contract: a plain
// text generation with reasoning_enabled=false. A model published with
// ReasoningToggle must compile this probe successfully; a ReasoningAlways
// model must reject it on the reasoning_enabled field. Keeping the probe
// here (instead of in every driver's tests) gives the contract one shared
// spelling across providers.
func ReasoningOffProbe() inference.GenerateRequest {
	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "probe"}},
				},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
	}
	disabled := false
	request.Input.Content.Intent.Text.ReasoningEnabled = &disabled
	return request
}
