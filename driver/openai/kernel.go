package openai

// Kernel bindings: the operation drivers in this package speak the OpenAI
// wire protocol, which Azure OpenAI shares endpoint-for-endpoint. Rather
// than forking the drivers, sibling providers bind them through the Kernel*
// helpers below and supply their own credentials, endpoint rewriting, and
// capability declarations. Everything stays inspectable: the wire models,
// ledgers, and error classification are exactly the openai provider's.

import (
	"github.com/GizClaw/flowcraft/core/inference"

	openaigo "github.com/openai/openai-go/v3"
)

// Capabilities declares what one externally managed deployment accepts.
// The zero value is the bare text surface: compilers reject every channel
// the declaration omits, so a deployment never silently accepts a feature
// its backing model may not serve.
type Capabilities struct {
	// Vision accepts image input parts on generate.
	Vision bool
	// Reasoning accepts the reasoning effort and summary knobs.
	Reasoning bool
	// WebSearch accepts the hosted web_search tool on generate.
	WebSearch bool
	// Dimensions accepts the embed dimensions knob.
	Dimensions bool
}

// KernelGenerate binds the chat driver (Responses API, unary + stream) for
// one externally managed deployment.
func KernelGenerate(
	client openaigo.Client,
	model string,
	caps Capabilities,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileGenerate(model, catalogEntry{
			kind:      kindGenerate,
			vision:    caps.Vision,
			reasoning: caps.Reasoning,
			webSearch: caps.WebSearch,
		}),
		transportGenerate(client),
		decodeGenerate,
		transportGenerateStream(client),
		decodeGenerateStream,
	)
}

// KernelChatGenerate binds the chat-completions pipeline (unary +
// stream) for one deployment. It shares the generate compiler and
// canonical decode with the Responses kernel; only the transport and
// streaming adapter differ.
func KernelChatGenerate(
	client openaigo.Client,
	model string,
	caps Capabilities,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileGenerate(model, catalogEntry{
			kind:      kindGenerate,
			api:       apiChat,
			vision:    caps.Vision,
			reasoning: caps.Reasoning,
		}),
		transportChatGenerate(client),
		decodeGenerate,
		transportChatGenerateStream(client),
		decodeChatGenerateStream,
	)
}

// KernelEmbed binds the embeddings driver for one deployment.
func KernelEmbed(
	client openaigo.Client,
	model string,
	caps Capabilities,
) (inference.EmbedDriver, error) {
	return inference.BindEmbed(
		compileEmbed(model, catalogEntry{
			kind:       kindEmbed,
			dimensions: caps.Dimensions,
		}),
		transportEmbed(client),
		decodeEmbed,
	)
}

// KernelImage binds the image generation driver (unary only) for one
// deployment.
func KernelImage(
	client openaigo.Client,
	model string,
) (inference.GenerateOperations, error) {
	unary, err := inference.BindGenerate(
		compileImage(model),
		transportImage(client),
		decodeImage,
	)
	if err != nil {
		return inference.GenerateOperations{}, err
	}
	return inference.GenerateOperations{Unary: unary}, nil
}

// KernelTTS binds the speech synthesis driver (unary + raw byte stream)
// for one deployment.
func KernelTTS(
	client openaigo.Client,
	model string,
) (inference.GenerateOperations, error) {
	return inference.BindGenerateOperations(
		compileTTS(model),
		transportTTS(client),
		decodeTTS,
		transportTTSStream(client),
		decodeTTSStream,
	)
}
