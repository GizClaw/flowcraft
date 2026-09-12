package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/utils/ptr"
)

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
	return providerID
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
	// JSONSet carries request-body fields this driver does not model, for
	// compatible endpoints whose dialect extends the OpenAI schema (Kimi's
	// `thinking`, Qwen's `enable_thinking` / `thinking_budget`, a gateway's
	// own knob). Each key is a path in sjson notation — `enable_thinking`
	// sets a top-level field, `thinking.keep` sets one leaf and leaves its
	// siblings alone — and each value is the raw JSON to place there.
	//
	// This is unverified passthrough, not a capability claim: FlowCraft
	// cannot validate what an endpoint does with a field it did not model,
	// so a deployment that needs it says so here, explicitly, per request.
	// The compile report names every key it applied, which is what keeps the
	// ledger from claiming a setting the provider never received.
	//
	// Keys naming a field the compiler already lowers from the canonical
	// request (model, messages, tools, reasoning, store, ...) are rejected:
	// the typed knob and the ledger are the only honest way to set those.
	JSONSet map[string]json.RawMessage `json:"json_set,omitempty"`
}

// JSONSet limits bound what one request may inject. The escape hatch is for
// a handful of provider knobs, not for smuggling a payload past the ledger,
// and the request report carries one decision per key.
const (
	maxJSONSetKeys  = 32
	maxJSONSetBytes = 64 << 10
)

// jsonSetReservedRoots are the body fields the compiler owns. A root here
// either carries canonical request data the compiler lowered (model,
// messages, tools, reasoning, store, the output-shape knobs) or changes the
// response contract the decoder assumes (n, modalities, audio). A JSONSet key
// under one of them is a configuration error, not a passthrough: letting it
// through would make the compile report claim a decision the body contradicts.
var jsonSetReservedRoots = []string{
	"model", "messages", "input", "instructions",
	"stream", "stream_options",
	"tools", "tool_choice", "functions", "function_call",
	"response_format", "text",
	"max_tokens", "max_completion_tokens", "max_output_tokens",
	"temperature", "top_p", "n",
	"store", "metadata", "reasoning", "reasoning_effort",
	"service_tier", "verbosity", "parallel_tool_calls", "max_tool_calls",
	"safety_identifier", "prompt_cache_key",
	"modalities", "audio",
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
	fields = append(fields, jsonSetFields(o.JSONSet)...)
	return fields
}

// jsonSetFields names one active field per injected body path, sorted so the
// compile report does not inherit Go's map iteration order. The ledger's field
// vocabulary is a flat identifier, so a path is flattened with the same
// character set extension ids use ("thinking.keep" → "thinking_keep"); two
// paths that flatten alike share one decision rather than tripping the
// duplicate-field check.
func jsonSetFields(entries map[string]json.RawMessage) []inference.ExtensionField {
	if len(entries) == 0 {
		return nil
	}
	names := make([]string, 0, len(entries))
	seen := make(map[string]struct{}, len(entries))
	for path := range entries {
		name := jsonSetFieldName(path)
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	fields := make([]inference.ExtensionField, 0, len(names))
	for _, name := range names {
		fields = append(fields, inference.ExtensionField(name))
	}
	return fields
}

// jsonSetFieldName flattens one sjson path into the ledger's identifier
// vocabulary. Paths that start with a character an identifier may not start
// with are prefixed so the name stays valid.
func jsonSetFieldName(path string) string {
	var builder strings.Builder
	builder.Grow(len(path))
	for _, char := range path {
		switch {
		case char >= 'a' && char <= 'z',
			char >= 'A' && char <= 'Z',
			char >= '0' && char <= '9',
			char == '_', char == '-':
			builder.WriteRune(char)
		default:
			builder.WriteRune('_')
		}
	}
	name := builder.String()
	if name == "" {
		return "field"
	}
	first := rune(name[0])
	if (first >= 'a' && first <= 'z') ||
		(first >= 'A' && first <= 'Z') ||
		(first >= '0' && first <= '9') {
		return name
	}
	return "field" + name
}

func (o GenerateOptions) Validate() error {
	if err := validateBodyFields("json_set", o.JSONSet); err != nil {
		return err
	}
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

// validateBodyFields enforces the contract shared by the two ways to name a
// body field this driver does not model — the per-request json_set extension
// and the deployment-level spec.wire.extra_body: a bounded number of bounded,
// well-formed values, none of them under a body field the compiler owns.
// label names the configuration surface in the error.
func validateBodyFields(label string, entries map[string]json.RawMessage) error {
	if len(entries) == 0 {
		return nil
	}
	if len(entries) > maxJSONSetKeys {
		return fmt.Errorf(
			"%s carries %d keys, at most %d are allowed",
			label, len(entries), maxJSONSetKeys,
		)
	}
	paths := make([]string, 0, len(entries))
	for path := range entries {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	total := 0
	for _, path := range paths {
		value := entries[path]
		switch {
		case strings.TrimSpace(path) == "":
			return fmt.Errorf("%s has an empty key", label)
		case !json.Valid(value) || len(bytes.TrimSpace(value)) == 0:
			return fmt.Errorf("%s %q is not a JSON value", label, path)
		case slices.Contains(jsonSetReservedRoots, jsonSetRoot(path)):
			return fmt.Errorf(
				"%s %q names a field the compiler lowers from the "+
					"canonical request; use the typed knob or extension field instead",
				label, path,
			)
		}
		total += len(value)
	}
	if total > maxJSONSetBytes {
		return fmt.Errorf(
			"%s carries %d bytes, at most %d are allowed",
			label, total, maxJSONSetBytes,
		)
	}
	return nil
}

// jsonSetRoot returns the body field a path addresses: the first sjson
// segment, with escaped dots kept inside the segment. Bracket segments
// (`messages[0]`) count as a new segment for the same reason a dot does, so a
// key can never reach under a reserved field by another spelling.
func jsonSetRoot(path string) string {
	escaped := false
	for index, char := range path {
		if escaped {
			escaped = false
			continue
		}
		switch char {
		case '\\':
			escaped = true
		case '.', '[':
			return path[:index]
		}
	}
	return path
}

func (o GenerateOptions) Clone() inference.Extension {
	o.ParallelToolCalls = ptr.Clone(o.ParallelToolCalls)
	o.MaxToolCalls = ptr.Clone(o.MaxToolCalls)
	o.JSONSet = cloneJSONSet(o.JSONSet)
	if o.WebSearch != nil {
		search := *o.WebSearch
		search.AllowedDomains = append([]string(nil), search.AllowedDomains...)
		search.ExternalWebAccess = ptr.Clone(search.ExternalWebAccess)
		search.ToolChoice = ptr.Clone(search.ToolChoice)
		o.WebSearch = &search
	}
	return o
}

// cloneJSONSet copies the injected body values: the extension is cloned onto
// every attempt, and an attempt must not hand a caller's backing array to the
// next one.
func cloneJSONSet(entries map[string]json.RawMessage) map[string]json.RawMessage {
	if entries == nil {
		return nil
	}
	cloned := make(map[string]json.RawMessage, len(entries))
	for path, value := range entries {
		cloned[path] = bytes.Clone(value)
	}
	return cloned
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
	o.PartialImages = ptr.Clone(o.PartialImages)
	return o
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
	o.Instructions = ptr.Clone(o.Instructions)
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
