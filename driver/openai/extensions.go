package openai

import (
	"fmt"
	"slices"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// driverID namespaces every extension this package defines.
const driverID = "openai"

const (
	extensionGenerate = "generate_options"
	extensionImage    = "image_options"
	extensionTTS      = "tts_options"
)

// extensionProvider resolves the deployment provider ID an extension targets,
// defaulting to the driver name.
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

// GenerateOptions carries OpenAI Responses API settings that have no
// canonical representation.
type GenerateOptions struct {
	// Provider targets a deployment provider ID other than "openai".
	Provider string `json:"-"`
	// WebSearch attaches OpenAI's hosted web_search tool.
	WebSearch *GenerateWebSearch `json:"web_search,omitempty"`
	// ServiceTier selects the processing tier for this call: "auto",
	// "default", "flex", "scale", or "priority". Empty keeps the provider
	// default.
	ServiceTier string `json:"service_tier,omitempty"`
	// ParallelToolCalls allows the model to issue tool calls in parallel.
	// Nil keeps the provider default (allowed).
	ParallelToolCalls *bool `json:"parallel_tool_calls,omitempty"`
	// MaxToolCalls caps the tool calls one response may request. Responses
	// only; nil keeps the provider default.
	MaxToolCalls *int `json:"max_tool_calls,omitempty"`
	// Verbosity tunes how much the model writes: "low", "medium", or
	// "high". Responses only; empty keeps the provider default.
	Verbosity string `json:"verbosity,omitempty"`
	// SafetyIdentifier is a stable, privacy-preserving identifier for the
	// end user, used by the provider for abuse monitoring. Send a hash, not
	// an email or account id.
	SafetyIdentifier string `json:"safety_identifier,omitempty"`
	// PromptCacheKey names the conversation prefix this request extends, so
	// an endpoint with explicit cache routing targets the right cache. It is
	// OpenAI-wire vocabulary rather than a canonical concept: endpoints that
	// manage caching themselves leave it unset.
	PromptCacheKey string `json:"prompt_cache_key,omitempty"`
}

// GenerateWebSearch configures the hosted web_search tool.
type GenerateWebSearch struct {
	// SearchContextSize controls how much web search context the model can
	// consume: "low", "medium", or "high". Empty keeps the provider default.
	SearchContextSize string `json:"search_context_size,omitempty"`
	// AllowedDomains restricts search results to the listed domains and their
	// subdomains. Empty allows all domains.
	AllowedDomains []string `json:"allowed_domains,omitempty"`
	// UserLocation localizes results.
	UserLocation GenerateWebSearchLocation `json:"user_location,omitempty"`
	// ExternalWebAccess controls whether the model may load pages that are not
	// directly search-engine results.
	ExternalWebAccess *bool `json:"external_web_access,omitempty"`
	// ReturnTokenBudget controls the returned-token budget for reasoning web
	// search runs: "default" or "unlimited".
	ReturnTokenBudget string `json:"return_token_budget,omitempty"`
	// ToolChoice controls whether search is optional (auto) or mandatory
	// (required). Nil behaves as auto.
	ToolChoice *GenerateWebSearchToolChoice `json:"tool_choice,omitempty"`
}

// GenerateWebSearchToolChoice selects the web search tool choice mode.
type GenerateWebSearchToolChoice struct {
	// Required forces the model to run web search when true.
	Required bool `json:"required"`
}

// GenerateWebSearchLocation is the approximate location for web search.
type GenerateWebSearchLocation struct {
	City     string `json:"city,omitempty"`
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

func (o GenerateOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o GenerateOptions) ExtensionID() string { return extensionGenerate }

func (o GenerateOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.ServiceTier != "" {
		fields = append(fields, "service_tier")
	}
	if o.ParallelToolCalls != nil {
		fields = append(fields, "parallel_tool_calls")
	}
	if o.MaxToolCalls != nil {
		fields = append(fields, "max_tool_calls")
	}
	if o.Verbosity != "" {
		fields = append(fields, "verbosity")
	}
	if o.SafetyIdentifier != "" {
		fields = append(fields, "safety_identifier")
	}
	if o.PromptCacheKey != "" {
		fields = append(fields, "prompt_cache_key")
	}
	if o.WebSearch != nil {
		fields = append(fields, "web_search")
		if o.WebSearch.ToolChoice != nil {
			fields = append(fields, "web_search_tool_choice")
		}
	}
	return fields
}

func (o GenerateOptions) Validate() error {
	if o.ServiceTier != "" && !validServiceTier(o.ServiceTier) {
		return fmt.Errorf(
			"service_tier %q is not one of auto/default/flex/scale/priority",
			o.ServiceTier,
		)
	}
	if calls := o.MaxToolCalls; calls != nil && *calls <= 0 {
		return fmt.Errorf("max_tool_calls must be positive, not %d", *calls)
	}
	if o.Verbosity != "" && !validVerbosity(o.Verbosity) {
		return fmt.Errorf("verbosity %q is not low/medium/high", o.Verbosity)
	}
	if search := o.WebSearch; search != nil {
		switch search.SearchContextSize {
		case "", "low", "medium", "high":
		default:
			return fmt.Errorf(
				"web_search search_context_size must be low, medium, or high, not %q",
				search.SearchContextSize,
			)
		}
		switch search.ReturnTokenBudget {
		case "", "default", "unlimited":
		default:
			return fmt.Errorf(
				"web_search return_token_budget must be default or unlimited, not %q",
				search.ReturnTokenBudget,
			)
		}
		if slices.Contains(search.AllowedDomains, "") {
			return fmt.Errorf("web_search allowed_domains entries must not be empty")
		}
	}
	return nil
}

func (o GenerateOptions) Clone() inference.Extension {
	o.ParallelToolCalls = clonePointer(o.ParallelToolCalls)
	o.MaxToolCalls = clonePointer(o.MaxToolCalls)
	if o.WebSearch != nil {
		search := *o.WebSearch
		search.AllowedDomains = append([]string(nil), search.AllowedDomains...)
		search.ExternalWebAccess = clonePointer(search.ExternalWebAccess)
		search.ToolChoice = clonePointer(search.ToolChoice)
		o.WebSearch = &search
	}
	return o
}

// ---------------------------------------------------------------------------
// Image (gpt-image).
// ---------------------------------------------------------------------------

// ImageOptions carries images API settings beyond the canonical image
// intent.
type ImageOptions struct {
	// Provider targets a deployment provider ID other than "openai".
	Provider string `json:"-"`
	// Mask is an inline PNG whose fully transparent areas (alpha zero) mark
	// where the first reference image should be edited (local inpainting).
	// Deployment-routed endpoints (endpoint.routing "azure_deployment") apply
	// the mask to the first reference image only, and it must be an inline
	// PNG with the same dimensions as that image because images/edits
	// uploads multipart files. Plain OpenAI endpoints reject the field at
	// compile time.
	Mask *media.ImageSource `json:"mask,omitempty"`
	// PartialImages sets the number of progress previews streamed before
	// the final image when the request runs in the stream execution shape.
	// It must be between 0 and 3: 0 delivers the final image in a single
	// stream event, and 1-3 deliver interim previews as they are generated.
	// Nil keeps the provider default (0). It has no effect on the unary
	// execution shape.
	PartialImages *int `json:"partial_images,omitempty"`
}

func (o ImageOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o ImageOptions) ExtensionID() string { return extensionImage }

func (o ImageOptions) ActiveFields() []inference.ExtensionField {
	var fields []inference.ExtensionField
	if o.Mask != nil {
		fields = append(fields, "mask")
	}
	if o.PartialImages != nil {
		fields = append(fields, "partial_images")
	}
	return fields
}

func (o ImageOptions) Validate() error {
	if mask := o.Mask; mask != nil {
		if base := mask.BaseMediaType(); base != "image/png" {
			return fmt.Errorf("mask must be a PNG image, not %q", base)
		}
	}
	if partial := o.PartialImages; partial != nil {
		if *partial < 0 || *partial > 3 {
			return fmt.Errorf(
				"partial_images must be between 0 and 3, not %d",
				*partial,
			)
		}
	}
	return nil
}

func (o ImageOptions) Clone() inference.Extension {
	if o.Mask != nil {
		mask := o.Mask.Clone()
		o.Mask = &mask
	}
	o.PartialImages = clonePointer(o.PartialImages)
	return o
}

// ---------------------------------------------------------------------------
// Consumption helpers.
// ---------------------------------------------------------------------------

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

func rejectOtherExtensions(
	operation string,
	other []inference.Extension,
	ledger *ledger,
) {
	for _, extension := range other {
		reason := fmt.Sprintf(
			"extension %q does not apply to %s",
			extension.ExtensionID(),
			operation,
		)
		for _, field := range extension.ActiveFields() {
			ledger.reject(field.Qualify(extension), reason)
		}
	}
}

// ---------------------------------------------------------------------------
// Audio (speech synthesis).
// ---------------------------------------------------------------------------

// TTSOptions carries speech settings beyond the canonical audio intent.
type TTSOptions struct {
	// Provider targets a deployment provider ID other than "openai".
	Provider string `json:"-"`
	// Instructions steers delivery — tone, pacing, accent — for models that
	// accept free-form style direction (gpt-4o-mini-tts and friends).
	Instructions *string `json:"instructions,omitempty"`
}

func (o TTSOptions) ProviderID() string  { return extensionProvider(o.Provider) }
func (o TTSOptions) ExtensionID() string { return extensionTTS }

func (o TTSOptions) ActiveFields() []inference.ExtensionField {
	if o.Instructions == nil {
		return nil
	}
	return []inference.ExtensionField{"instructions"}
}

func (o TTSOptions) Validate() error {
	if instructions := o.Instructions; instructions != nil &&
		strings.TrimSpace(*instructions) == "" {
		return fmt.Errorf("instructions must not be blank")
	}
	return nil
}

func (o TTSOptions) Clone() inference.Extension {
	o.Instructions = clonePointer(o.Instructions)
	return o
}

// validServiceTier reports whether value names a documented processing tier.
func validServiceTier(value string) bool {
	switch value {
	case "auto", "default", "flex", "scale", "priority":
		return true
	default:
		return false
	}
}

// validVerbosity reports whether value names a documented output length.
func validVerbosity(value string) bool {
	switch value {
	case "low", "medium", "high":
		return true
	default:
		return false
	}
}
