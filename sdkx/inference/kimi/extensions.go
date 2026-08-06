package kimi

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/inference"
)

// driverID is the provider id extensions target by default. Deployments
// that rename the provider set Provider on each extension to match.
const driverID = "kimi"

func extensionProvider(provider string) string {
	if provider != "" {
		return provider
	}
	return driverID
}

func clonePointer[T any](value *T) *T {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

const extensionGenerate = "generate_options"

// GenerateOptions carries Kimi-specific request settings that have no
// canonical representation.
type GenerateOptions struct {
	// Provider targets a deployment provider ID other than "kimi".
	Provider string `json:"-"`
	// PromptCacheKey rides Kimi's prompt_cache_key: a stable session or
	// task id that improves prompt-cache hit rates across requests.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
	// PreserveThinking overrides the compiler's Preserved-Thinking default
	// on models with an optional thinking.keep (kimi-k2.6): false forces
	// keep off even when history carries reasoning, true forces it on.
	PreserveThinking *bool `json:"preserve_thinking,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.PromptCacheKey != "" {
		fields = append(fields, "prompt_cache_key")
	}
	if o.PreserveThinking != nil {
		fields = append(fields, "preserve_thinking")
	}
	return fields
}

func (o GenerateOptions) Validate() error { return nil }

func (o GenerateOptions) Clone() inference.Extension {
	o.PreserveThinking = clonePointer(o.PreserveThinking)
	return o
}

// operationExtensions splits the request's extensions into the option
// struct for this operation and every other extension.
func operationExtensions[Options inference.Extension](
	extensions inference.Extensions,
) (Options, []inference.Extension) {
	var options Options
	var other []inference.Extension
	for _, extension := range extensions {
		if typed, ok := extension.(Options); ok {
			options = typed
		} else {
			other = append(other, extension)
		}
	}
	return options, other
}

// rejectOtherExtensions marks every active field of every extension that
// does not apply to this operation as rejected on the ledger.
func rejectOtherExtensions(operation string, other []inference.Extension, ledger *ledger) {
	for _, extension := range other {
		for _, field := range extension.ActiveFields() {
			ledger.reject(field.Qualify(extension), fmt.Sprintf("extension %q does not apply to %s", extension.ExtensionID(), operation))
		}
	}
}
