package openai

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/internal/ptr"
)

// GenerationExtensionID names this driver's per-call options extension. A
// deployment writes it inside an inference node's extensions list, which is what
// lets one provider instance serve nodes with different settings: the values are
// per call, not per provider.
const GenerationExtensionID = "generation"

// Effort is the canonical reasoning depth a caller asks for. The driver maps it
// onto the surface's own value, so a deployment never writes a wire token.
type Effort string

const (
	EffortMinimal Effort = "minimal"
	EffortLow     Effort = "low"
	EffortMedium  Effort = "medium"
	EffortHigh    Effort = "high"
	EffortXHigh   Effort = "xhigh"
)

func (e Effort) valid() bool {
	switch e {
	case "", EffortMinimal, EffortLow, EffortMedium, EffortHigh, EffortXHigh:
		return true
	default:
		return false
	}
}

// ResponseFormatKind names the structured-output modes.
type ResponseFormatKind string

const (
	FormatText       ResponseFormatKind = "text"
	FormatJSONObject ResponseFormatKind = "json_object"
	FormatJSONSchema ResponseFormatKind = "json_schema"
)

// ResponseFormat constrains the generated answer.
type ResponseFormat struct {
	Kind ResponseFormatKind `json:"kind" yaml:"kind"`
	// Name identifies the schema on the wire. Required for json_schema.
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
	// Schema is the JSON Schema document. Required for json_schema.
	Schema json.RawMessage `json:"schema,omitempty" yaml:"schema,omitempty"`
	// Strict asks the provider to enforce the schema rather than suggest it.
	Strict bool `json:"strict,omitempty" yaml:"strict,omitempty"`
}

func (f *ResponseFormat) validate() error {
	switch f.Kind {
	case FormatText, FormatJSONObject:
		return nil
	case FormatJSONSchema:
		if f.Name == "" {
			return fmt.Errorf("json_schema response requires a name")
		}
		if len(f.Schema) == 0 {
			return fmt.Errorf("json_schema response requires a schema")
		}
		if !json.Valid(f.Schema) {
			return fmt.Errorf("json_schema response schema is not valid JSON")
		}
		return nil
	default:
		return fmt.Errorf("unknown response format %q", f.Kind)
	}
}

// WebSearch requests the provider-hosted search tool.
type WebSearch struct {
	// ContextSize is "low", "medium" or "high".
	ContextSize string `json:"context_size,omitempty" yaml:"context_size,omitempty"`
	// AllowedDomains restricts results to these domains.
	AllowedDomains []string `json:"allowed_domains,omitempty" yaml:"allowed_domains,omitempty"`
}

// GenerationOptions carries every per-call knob this driver accepts that is not
// part of the canonical request. It is an extension rather than a request field
// because the values are neither provider-neutral in their wire spelling nor
// decided by a provider-agnostic caller: a deployment sets them per node, and the
// config layer decodes them through [Decoders].
//
// A field the selected wire surface cannot express is rejected during
// compilation with [inference.UnsupportedFeature]; nothing here is silently
// ignored.
type GenerationOptions struct {
	// Temperature is in [0, 2]; TopP is in [0, 1].
	Temperature *float64 `json:"temperature,omitempty" yaml:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty" yaml:"top_p,omitempty"`
	// MaxOutputTokens caps the generated answer.
	MaxOutputTokens *int `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`
	// ReasoningEffort asks for a reasoning depth where the model has levels.
	ReasoningEffort Effort `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	// ResponseFormat constrains the answer's shape.
	ResponseFormat *ResponseFormat `json:"response_format,omitempty" yaml:"response_format,omitempty"`
	// ServiceTier selects the provider's processing tier.
	ServiceTier string `json:"service_tier,omitempty" yaml:"service_tier,omitempty"`
	// ParallelToolCalls allows several tool calls per turn.
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
	// MaxToolCalls caps the tool calls one answer may request (Responses only).
	MaxToolCalls *int `json:"max_tool_calls,omitempty" yaml:"max_tool_calls,omitempty"`
	// Verbosity tunes answer length: low, medium or high (Responses only).
	Verbosity string `json:"verbosity,omitempty" yaml:"verbosity,omitempty"`
	// SafetyIdentifier is a stable, privacy-preserving end-user identifier for
	// the provider's abuse monitoring. Send a hash, never an account id.
	SafetyIdentifier string `json:"safety_identifier,omitempty" yaml:"safety_identifier,omitempty"`
	// PromptCacheKey names the conversation prefix this call extends, so an
	// endpoint with explicit cache routing targets the right cache.
	PromptCacheKey string `json:"prompt_cache_key,omitempty" yaml:"prompt_cache_key,omitempty"`
	// WebSearch requests the hosted search tool (Responses only).
	WebSearch *WebSearch `json:"web_search,omitempty" yaml:"web_search,omitempty"`
	// Store asks the provider to keep the answer for later retrieval
	// (Responses only). Nil leaves the provider default, which stores.
	Store *bool `json:"store,omitempty" yaml:"store,omitempty"`
	// JSONSet writes raw wire fields verbatim, for a provider quirk this
	// vocabulary does not model. Each key is reported as its own field so the
	// contract check still sees it; values are merged into the request body at
	// the transport, which is the only stage allowed to touch the wire.
	JSONSet map[string]json.RawMessage `json:"json_set,omitempty" yaml:"json_set,omitempty"`
}

// ProviderID implements [inference.Extension].
func (*GenerationOptions) ProviderID() string { return ProviderID }

// ExtensionID implements [inference.Extension].
func (*GenerationOptions) ExtensionID() string { return GenerationExtensionID }

// ActiveFields lists the options this value actually sets. The pipeline compares
// it against the request's active fields, so an option that is read but not
// listed, or listed but not read, is a driver bug rather than a silent drop.
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
	if o.MaxOutputTokens != nil {
		add("max_output_tokens")
	}
	if o.ReasoningEffort != "" {
		add("reasoning_effort")
	}
	if o.ResponseFormat != nil {
		add("response_format")
	}
	if o.ServiceTier != "" {
		add("service_tier")
	}
	if o.ParallelToolCalls != nil {
		add("parallel_tool_calls")
	}
	if o.MaxToolCalls != nil {
		add("max_tool_calls")
	}
	if o.Verbosity != "" {
		add("verbosity")
	}
	if o.SafetyIdentifier != "" {
		add("safety_identifier")
	}
	if o.PromptCacheKey != "" {
		add("prompt_cache_key")
	}
	if o.WebSearch != nil {
		add("web_search")
	}
	if o.Store != nil {
		add("store")
	}
	keys := make([]string, 0, len(o.JSONSet))
	for key := range o.JSONSet {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		add("json." + key)
	}
	return fields
}

// Validate checks the options' own invariants.
func (o *GenerationOptions) Validate() error {
	if o == nil {
		return nil
	}
	if o.Temperature != nil && (*o.Temperature < 0 || *o.Temperature > 2) {
		return fmt.Errorf("temperature must be within [0, 2]")
	}
	if o.TopP != nil && (*o.TopP < 0 || *o.TopP > 1) {
		return fmt.Errorf("top_p must be within [0, 1]")
	}
	if o.MaxOutputTokens != nil && *o.MaxOutputTokens <= 0 {
		return fmt.Errorf("max_output_tokens must be positive")
	}
	if o.MaxToolCalls != nil && *o.MaxToolCalls <= 0 {
		return fmt.Errorf("max_tool_calls must be positive")
	}
	if !o.ReasoningEffort.valid() {
		return fmt.Errorf("unknown reasoning effort %q", o.ReasoningEffort)
	}
	if o.ResponseFormat != nil {
		if err := o.ResponseFormat.validate(); err != nil {
			return err
		}
	}
	if o.Verbosity != "" && !slices.Contains([]string{"low", "medium", "high"}, o.Verbosity) {
		return fmt.Errorf("unknown verbosity %q", o.Verbosity)
	}
	for key, value := range o.JSONSet {
		if key == "" {
			return fmt.Errorf("json_set key must not be empty")
		}
		if !json.Valid(value) {
			return fmt.Errorf("json_set[%q] is not valid JSON", key)
		}
	}
	if o.WebSearch != nil && o.WebSearch.ContextSize != "" {
		if !slices.Contains([]string{"low", "medium", "high"}, o.WebSearch.ContextSize) {
			return fmt.Errorf("unknown web search context size %q", o.WebSearch.ContextSize)
		}
	}
	return nil
}

// Clone returns an independent copy.
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
	if o.MaxOutputTokens != nil {
		value := *o.MaxOutputTokens
		clone.MaxOutputTokens = &value
	}
	if o.MaxToolCalls != nil {
		value := *o.MaxToolCalls
		clone.MaxToolCalls = &value
	}
	if o.ParallelToolCalls != nil {
		value := *o.ParallelToolCalls
		clone.ParallelToolCalls = &value
	}
	if o.ResponseFormat != nil {
		format := *o.ResponseFormat
		format.Schema = append(json.RawMessage(nil), o.ResponseFormat.Schema...)
		clone.ResponseFormat = &format
	}
	if o.WebSearch != nil {
		search := *o.WebSearch
		search.AllowedDomains = append([]string(nil), o.WebSearch.AllowedDomains...)
		clone.WebSearch = &search
	}
	if o.Store != nil {
		value := *o.Store
		clone.Store = &value
	}
	if o.JSONSet != nil {
		set := make(map[string]json.RawMessage, len(o.JSONSet))
		for key, value := range o.JSONSet {
			set[key] = append(json.RawMessage(nil), value...)
		}
		clone.JSONSet = set
	}
	return &clone
}

// Decoders returns the config-side decoders for this driver's extensions, keyed
// "provider/id". A deployment's extension list is resolved through them, so an
// option named in config but unknown to this build fails instead of being
// ignored.
func Decoders() map[string]inference.ExtensionDecoder {
	return map[string]inference.ExtensionDecoder{
		ProviderID + "/" + GenerationExtensionID: decodeGeneration,
		ProviderID + "/" + ImageExtensionID:      decodeImage,
		ProviderID + "/" + SpeechExtensionID:     decodeSpeech,
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

// generationOptions extracts this driver's extension from a request. The pipeline
// has already dropped extensions addressed to other providers, so anything left
// that is not this type is a caller error worth reporting.
func generationOptions(extensions inference.Extensions) (*GenerationOptions, error) {
	var found *GenerationOptions
	for _, extension := range extensions {
		options, ok := extension.(*GenerationOptions)
		if !ok {
			return nil, inference.NewError(
				inference.InvalidExtension,
				model.OperationGenerate,
				inference.FieldExtensions,
				fmt.Errorf("openai: extension %s/%s is not understood by this driver",
					extension.ProviderID(), extension.ExtensionID()),
			)
		}
		if found != nil {
			return nil, inference.NewError(
				inference.InvalidExtension,
				model.OperationGenerate,
				inference.FieldExtensions,
				fmt.Errorf("openai: duplicate generation options"),
			)
		}
		found = options
	}
	return found, nil
}

// extensionFields returns the qualified identities of the options' active fields,
// for the compilation to report as honored.
func extensionFields(options *GenerationOptions) []inference.Field {
	if options == nil {
		return nil
	}
	return qualifiedFields(options)
}

// qualifiedFields turns an extension's local option names into the canonical field
// identities the contract check compares against.
func qualifiedFields(extension inference.Extension) []inference.Field {
	// A nil *T stored in an interface is not nil, and calling through it panics;
	// ptr.IsNil sees both forms.
	if ptr.IsNil(extension) {
		return nil
	}
	active := extension.ActiveFields()
	fields := make([]inference.Field, 0, len(active))
	for _, field := range active {
		fields = append(fields, field.Qualify(extension))
	}
	return fields
}

// extensionJSONSet exposes the injected raw fields to the transport. It is nil for
// every request that does not use the escape hatch.
func extensionJSONSet(extensions inference.Extensions) (map[string]json.RawMessage, error) {
	options, err := generationOptions(extensions)
	if err != nil {
		return nil, err
	}
	if options == nil {
		return nil, nil
	}
	return options.JSONSet, nil
}
