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
type Capabilities struct {
	// Vision accepts image input parts on generate.
	Vision bool
	// Reasoning accepts the reasoning effort knob and thinking traces.
	Reasoning bool
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
			vision:    caps.Vision,
			reasoning: caps.Reasoning,
		}),
		transportGenerate(client),
		decodeGenerate,
		transportGenerateStream(client),
		decodeGenerateStream,
	)
}
