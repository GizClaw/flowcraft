package anthropic

import (
	"fmt"

	anthropicgo "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/param"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/goccy/go-json"
)

// GenerationExtensionID names this driver's per-call options extension. A
// deployment writes it inside an inference node's extensions list, which is what
// lets one provider instance serve nodes with different settings.
const GenerationExtensionID = "generation"

// Thinking selects the Messages API's reasoning control. The API accepts one
// control per request: disabled, adaptive, an explicit token budget, or an
// effort level.
type Thinking struct {
	// Enabled switches thinking off when false and adaptive thinking on when
	// true. Nil leaves the model default alone.
	Enabled *bool `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	// BudgetTokens enables thinking with an explicit internal-reasoning budget.
	// It must be at least 1024 and less than max_output_tokens.
	BudgetTokens *int `json:"budget_tokens,omitempty" yaml:"budget_tokens,omitempty"`
	// Effort asks for a model-specific reasoning depth. It is mutually
	// exclusive with BudgetTokens.
	Effort Effort `json:"effort,omitempty" yaml:"effort,omitempty"`
}

// Effort is the reasoning depth the Messages API accepts.
type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

func (e Effort) valid() bool {
	switch e {
	case "", EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return true
	default:
		return false
	}
}

// ResponseFormat asks for structured output. The Messages API serves JSON
// schemas only; it has no json_object mode.
type ResponseFormat struct {
	// Schema is the JSON Schema document. It must be a JSON object.
	Schema json.RawMessage `json:"schema" yaml:"schema"`
}

func (f *ResponseFormat) validate() error {
	if len(f.Schema) == 0 {
		return fmt.Errorf("response format requires a schema")
	}
	var object map[string]any
	if err := json.Unmarshal(f.Schema, &object); err != nil {
		return fmt.Errorf("response format schema must be a JSON object: %w", err)
	}
	if object == nil {
		return fmt.Errorf("response format schema must be a JSON object")
	}
	return nil
}

// GenerationOptions carries the per-call options this driver accepts beyond the
// canonical request. Every field is optional; an unset field leaves the
// provider default alone.
type GenerationOptions struct {
	// Temperature is in [0, 1]; TopP is in [0, 1].
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	// TopK limits sampling to the k most likely tokens.
	TopK *int `json:"top_k,omitempty" yaml:"top_k,omitempty"`
	// MaxOutputTokens caps the generated answer. Empty uses DefaultMaxTokens.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	// Thinking selects the reasoning control. Nil leaves the model default.
	Thinking *Thinking `json:"thinking,omitempty" yaml:"thinking,omitempty"`
	// ResponseFormat constrains the answer to a JSON schema.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty" yaml:"response_format,omitempty"`
}

// ProviderID implements [inference.Extension].
func (*GenerationOptions) ProviderID() string { return ProviderID }

// ExtensionID implements [inference.Extension].
func (*GenerationOptions) ExtensionID() string { return GenerationExtensionID }

// ActiveFields lists the options this value actually sets.
func (o *GenerationOptions) ActiveFields() []inference.ExtensionField {
	if o == nil {
		return nil
	}
	var fields []inference.ExtensionField
	add := func(name string) { fields = append(fields, inference.ExtensionField(name)) }
	if o.Temperature != nil {
		add("temperature")
	}
	if o.TopP != nil {
		add("top_p")
	}
	if o.TopK != nil {
		add("top_k")
	}
	if o.MaxOutputTokens != nil {
		add("max_output_tokens")
	}
	if o.Thinking != nil {
		add("thinking")
	}
	if o.ResponseFormat != nil {
		add("response_format")
	}
	return fields
}

// Validate checks the options' own invariants.
func (o *GenerationOptions) Validate() error {
	if o == nil {
		return nil
	}
	if o.Temperature != nil && (*o.Temperature < 0 || *o.Temperature > 1) {
		return fmt.Errorf("temperature must be within [0, 1]")
	}
	if o.TopP != nil && (*o.TopP < 0 || *o.TopP > 1) {
		return fmt.Errorf("top_p must be within [0, 1]")
	}
	if o.TopK != nil && *o.TopK <= 0 {
		return fmt.Errorf("top_k must be positive")
	}
	if o.MaxOutputTokens != nil && *o.MaxOutputTokens <= 0 {
		return fmt.Errorf("max_output_tokens must be positive")
	}
	if o.Thinking != nil {
		if err := o.Thinking.validate(); err != nil {
			return err
		}
	}
	if o.ResponseFormat != nil {
		if err := o.ResponseFormat.validate(); err != nil {
			return err
		}
	}
	return nil
}

func (t *Thinking) validate() error {
	if t == nil {
		return nil
	}
	if !t.Effort.valid() {
		return fmt.Errorf("unknown thinking effort %q", t.Effort)
	}
	if t.BudgetTokens != nil {
		if *t.BudgetTokens < 1024 {
			return fmt.Errorf("thinking budget_tokens must be at least 1024")
		}
		if t.Effort != "" {
			return fmt.Errorf("thinking accepts budget_tokens or effort, not both")
		}
	}
	if t.Enabled != nil && !*t.Enabled &&
		(t.BudgetTokens != nil || t.Effort != "") {
		return fmt.Errorf("disabled thinking cannot carry a budget or effort")
	}
	return nil
}

// Clone returns a deep copy that shares no mutable state.
func (o *GenerationOptions) Clone() inference.Extension {
	if o == nil {
		return (*GenerationOptions)(nil)
	}
	clone := *o
	if o.Temperature != nil {
		value := *o.Temperature
		clone.Temperature = &value
	}
	if o.TopP != nil {
		value := *o.TopP
		clone.TopP = &value
	}
	if o.TopK != nil {
		value := *o.TopK
		clone.TopK = &value
	}
	if o.MaxOutputTokens != nil {
		value := *o.MaxOutputTokens
		clone.MaxOutputTokens = &value
	}
	if o.Thinking != nil {
		thinking := *o.Thinking
		if o.Thinking.Enabled != nil {
			value := *o.Thinking.Enabled
			thinking.Enabled = &value
		}
		if o.Thinking.BudgetTokens != nil {
			value := *o.Thinking.BudgetTokens
			thinking.BudgetTokens = &value
		}
		clone.Thinking = &thinking
	}
	if o.ResponseFormat != nil {
		format := *o.ResponseFormat
		format.Schema = append(json.RawMessage(nil), o.ResponseFormat.Schema...)
		clone.ResponseFormat = &format
	}
	return &clone
}

// Decoders returns the config-side decoders for this driver's extensions.
func Decoders() map[string]inference.ExtensionDecoder {
	return map[string]inference.ExtensionDecoder{
		ProviderID + "/" + GenerationExtensionID: decodeGeneration,
	}
}

func decodeGeneration(fields json.RawMessage) (inference.Extension, error) {
	options := &GenerationOptions{}
	if len(fields) > 0 {
		if err := inference.DecodeStrict(fields, options); err != nil {
			return nil, err
		}
	}
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return options, nil
}

func generationOptions(extensions inference.Extensions) (*GenerationOptions, error) {
	var found *GenerationOptions
	for _, extension := range extensions {
		options, ok := extension.(*GenerationOptions)
		if !ok {
			return nil, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("anthropic: extension %s/%s is not understood by this driver",
					extension.ProviderID(), extension.ExtensionID()),
			)
		}
		if found != nil {
			return nil, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions,
				fmt.Errorf("anthropic: duplicate generation options"),
			)
		}
		found = options
	}
	if found != nil {
		if err := found.Validate(); err != nil {
			return nil, inference.NewError(
				inference.InvalidExtension, inference.FieldExtensions, err,
			)
		}
	}
	return found, nil
}

func applyGenerationOptions(
	params *anthropicgo.MessageNewParams,
	options *GenerationOptions,
) error {
	if options == nil {
		return nil
	}
	if options.Temperature != nil {
		params.Temperature = param.NewOpt(*options.Temperature)
	}
	if options.TopP != nil {
		params.TopP = param.NewOpt(*options.TopP)
	}
	if options.TopK != nil {
		params.TopK = param.NewOpt(int64(*options.TopK))
	}
	if options.MaxOutputTokens != nil {
		params.MaxTokens = int64(*options.MaxOutputTokens)
	}
	if options.Thinking != nil {
		thinking := options.Thinking
		if thinking.BudgetTokens != nil {
			if int64(*thinking.BudgetTokens) >= params.MaxTokens {
				return inference.NewError(
					inference.InvalidExtension, inference.FieldExtensions,
					fmt.Errorf("anthropic: thinking budget_tokens %d must be less than max_tokens %d",
						*thinking.BudgetTokens, params.MaxTokens),
				)
			}
			params.Thinking = anthropicgo.ThinkingConfigParamOfEnabled(
				int64(*thinking.BudgetTokens))
		} else if thinking.Effort != "" {
			params.OutputConfig.Effort = anthropicgo.OutputConfigEffort(thinking.Effort)
		} else if thinking.Enabled != nil {
			if *thinking.Enabled {
				params.Thinking = anthropicgo.ThinkingConfigParamUnion{
					OfAdaptive: &anthropicgo.ThinkingConfigAdaptiveParam{},
				}
			} else {
				params.Thinking = anthropicgo.ThinkingConfigParamUnion{
					OfDisabled: &anthropicgo.ThinkingConfigDisabledParam{},
				}
			}
		}
	}
	if options.ResponseFormat != nil {
		params.OutputConfig.Format = anthropicgo.JSONOutputFormatParam{
			Schema: schemaMap(options.ResponseFormat.Schema),
		}
	}
	return nil
}
