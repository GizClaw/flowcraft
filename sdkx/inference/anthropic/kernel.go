package anthropic

// Kernel bindings: the generate driver in this package speaks the Anthropic
// Messages protocol, which sibling Claude platforms (AWS Bedrock, Google
// Vertex AI) serve endpoint-for-endpoint behind their own authentication.
// Rather than forking the driver, those providers bind it through the
// Kernel* helpers below and supply their own credentials and capability
// declarations. Everything stays inspectable: the wire model, ledgers, and
// error classification are exactly the anthropic provider's.

import (
	"github.com/GizClaw/flowcraft/sdk/inference"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
)

// Capabilities declares what one externally managed Claude deployment
// accepts. The zero value is the bare text surface: the compiler rejects
// every channel the declaration omits, so a deployment never silently
// accepts a feature its backing model may not serve.
// ReasoningControl names the wire shape one Messages-compatible platform
// uses for the reasoning intent. Platforms genuinely differ here — even
// Anthropic's own API grew more than one shape — so the kernel declares
// the dialect instead of hardcoding Anthropic's.
type ReasoningControl int

const (
	// ReasoningControlEffort emits output_config.effort (low/medium/
	// high) — Anthropic's native surface.
	ReasoningControlEffort ReasoningControl = iota
	// ReasoningControlAdaptive emits thinking: {type: "adaptive"} — the
	// binary-thinking dialect of platforms whose thinking control has no
	// effort levels (MiniMax). Any requested effort turns thinking on;
	// the platform picks the depth.
	ReasoningControlAdaptive
)

type Capabilities struct {
	// Vision accepts image input parts on generate.
	Vision bool
	// Reasoning accepts the reasoning controls and thinking traces.
	Reasoning bool
	// ReasoningLevels honors effort levels exactly. Binary-thinking
	// platforms leave it false: the compiler turns any requested effort
	// into thinking-on and drops the level with a report.
	ReasoningLevels bool
	// ReasoningDisable can force thinking off. Platforms whose models
	// always think (MiniMax M2.x) leave it false and reject the switch.
	ReasoningDisable bool
	// ReasoningControl selects the wire shape for the reasoning intent.
	// The zero value is Anthropic's native output_config.effort.
	ReasoningControl ReasoningControl
}

// KernelGenerate binds the chat driver (Messages API, unary + stream) for
// one externally managed Claude deployment. The model string is whatever
// the platform expects on the wire — catalog name, Bedrock model id, or
// Vertex model@version — the kernel does not interpret it.
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
