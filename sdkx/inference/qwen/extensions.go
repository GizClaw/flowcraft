package qwen

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// Provider-specific settings ride on canonical requests as typed extensions
// (inference.Extension). Each operation family owns one options struct; the
// compiler for that operation consumes it field by field, and any extension
// attached to a request for a different operation is rejected with
// InvalidExtension.
//
// Field names are flat because extension field names may not contain dots.

// driverID namespaces every extension this package defines. The runtime
// qualifies extension fields with ProviderID and rejects extensions whose
// provider does not match the resolved model's deployment provider, so a
// deployment that names its provider differently must set the Provider field
// on the options structs it attaches.
const driverID = "qwen"

const (
	extensionGenerate = "generate_options"
	extensionEmbed    = "embed_options"
)

// extensionProvider resolves the deployment provider ID an extension
// targets, defaulting to the driver name.
func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return driverID
}

// GenerateOptions carries DashScope generation settings that have no
// canonical representation.
type GenerateOptions struct {
	// Provider targets a deployment provider ID other than "qwen".
	// Attempts for any other provider leave the extension inert rather
	// than rejecting it, so mixed-provider routes keep working.
	Provider string `json:"-"`
	// ThinkingBudget bounds the thinking trace length on hybrid-thinking
	// models without an effort control.
	ThinkingBudget *int64 `json:"thinking_budget,omitempty"`
	// PreserveThinking overrides whether assistant reasoning_content
	// history is re-ingested; unset means the compiler decides (on when
	// the history carries reasoning and the model supports it).
	PreserveThinking *bool `json:"preserve_thinking,omitempty"`
	// TopK bounds the sampling candidate pool.
	TopK *int64 `json:"top_k,omitempty"`
	// RepetitionPenalty penalizes repeated sequences; 1.0 disables.
	RepetitionPenalty *float64 `json:"repetition_penalty,omitempty"`
	// PresencePenalty penalizes tokens already present in the text,
	// [-2, 2].
	PresencePenalty *float64 `json:"presence_penalty,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.ThinkingBudget != nil {
		fields = append(fields, "thinking_budget")
	}
	if o.PreserveThinking != nil {
		fields = append(fields, "preserve_thinking")
	}
	if o.TopK != nil {
		fields = append(fields, "top_k")
	}
	if o.RepetitionPenalty != nil {
		fields = append(fields, "repetition_penalty")
	}
	if o.PresencePenalty != nil {
		fields = append(fields, "presence_penalty")
	}
	return fields
}

func (o GenerateOptions) Validate() error {
	if o.ThinkingBudget != nil && *o.ThinkingBudget <= 0 {
		return fmt.Errorf("thinking_budget must be positive")
	}
	if o.TopK != nil && *o.TopK < 0 {
		return fmt.Errorf("top_k must not be negative")
	}
	if o.RepetitionPenalty != nil && *o.RepetitionPenalty <= 0 {
		return fmt.Errorf("repetition_penalty must be positive")
	}
	if o.PresencePenalty != nil &&
		(*o.PresencePenalty < -2 || *o.PresencePenalty > 2) {
		return fmt.Errorf("presence_penalty must be within [-2, 2]")
	}
	return nil
}

func (o GenerateOptions) Clone() inference.Extension {
	o.ThinkingBudget = clonePointer(o.ThinkingBudget)
	o.PreserveThinking = clonePointer(o.PreserveThinking)
	o.TopK = clonePointer(o.TopK)
	o.RepetitionPenalty = clonePointer(o.RepetitionPenalty)
	o.PresencePenalty = clonePointer(o.PresencePenalty)
	return o
}

// EmbedOptions carries DashScope embedding settings that have no canonical
// representation. The task instruction applies to both embed models; the
// query/document asymmetry exists on text-embedding only.
type EmbedOptions struct {
	// Provider targets a deployment provider ID other than "qwen".
	Provider string `json:"-"`
	// TextType marks the input as a retrieval query or a corpus document
	// (text-embedding only): "query" or "document".
	TextType string `json:"text_type,omitempty"`
	// Instruct is a custom task instruction that steers the embedding
	// towards the downstream task; English works best.
	Instruct string `json:"instruct,omitempty"`
}

func (o EmbedOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o EmbedOptions) ExtensionID() string { return extensionEmbed }

func (o EmbedOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.TextType != "" {
		fields = append(fields, "text_type")
	}
	if o.Instruct != "" {
		fields = append(fields, "instruct")
	}
	return fields
}

func (o EmbedOptions) Validate() error {
	switch o.TextType {
	case "", "query", "document":
	default:
		return fmt.Errorf("text_type must be %q or %q", "query", "document")
	}
	return nil
}

func (o EmbedOptions) Clone() inference.Extension { return o }

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

// operationExtensions splits the request's extensions into the options
// struct this operation consumes and everything else.
func operationExtensions[T inference.Extension](
	extensions inference.Extensions,
) (T, []inference.Extension) {
	var options T
	var other []inference.Extension
	for _, extension := range extensions {
		if extension == nil {
			continue
		}
		if typed, ok := extension.(T); ok {
			options = typed
			continue
		}
		other = append(other, extension)
	}
	return options, other
}

// rejectOtherExtensions records a rejection for every active field of
// extensions that do not apply to the operation being compiled.
func rejectOtherExtensions(
	operation string,
	other []inference.Extension,
	ledger *ledger,
) {
	for _, extension := range other {
		if extension == nil {
			continue
		}
		for _, field := range extension.ActiveFields() {
			ledger.reject(
				field.Qualify(extension),
				fmt.Sprintf("%s does not consume %s", operation, extension.ExtensionID()),
			)
		}
	}
}
