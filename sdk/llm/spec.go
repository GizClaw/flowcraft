package llm

import "maps"

// ModelSpec is the complete model-fixed property set for one model:
// capability claims, default per-call parameters, and numeric hard
// limits. The same value type is used for catalog declarations
// (provider author, via ModelInfo.Spec) and deployment overrides
// (ProviderConfig.SpecOverride / ModelConfig.SpecOverride). Merging
// is field-wise via mergeSpec; non-zero override fields win.
//
// All three fields are independently optional. A zero-value ModelSpec
// declares "no claims": all capabilities supported, no defaults, no
// limits — same observable behavior as the pre-Spec era.
type ModelSpec struct {
	// Caps disables capabilities the model does not support. Black-list
	// semantics — zero value means "supports everything".
	Caps ModelCaps `json:"caps,omitempty" yaml:"caps,omitempty"`

	// Defaults supplies default GenerateOptions field values used when
	// the caller does not set a field explicitly. nil pointers and
	// empty slices in Defaults are treated as "no default declared"
	// (i.e. the field passes through unchanged).
	//
	// Caller-supplied options always win; defaults only fill gaps.
	Defaults GenerateOptions `json:"defaults,omitempty" yaml:"defaults,omitempty"`

	// Limits declares numeric hard ceilings the runtime enforces by
	// clamping (with a telemetry warning), not by erroring. Zero
	// values mean "no limit declared".
	Limits ModelLimits `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// IsZero reports whether the spec carries no caps, no defaults, and
// no limits. Used by the resolver to skip the wrap step entirely.
func (s ModelSpec) IsZero() bool {
	return s.Caps.IsZero() && isZeroOptions(s.Defaults) && s.Limits.IsZero()
}

// ModelLimits enumerates numeric hard ceilings for one model. All
// zero values mean "no limit". See doc/sdk-llm-redesign.md §3.1 for
// rationale.
type ModelLimits struct {
	// MaxOutputTokens caps GenerateOptions.MaxTokens. Caller values
	// exceeding this are clamped down (with telemetry.Warn). Caller
	// values left nil stay nil.
	MaxOutputTokens int64 `json:"max_output_tokens,omitempty" yaml:"max_output_tokens,omitempty"`

	// MaxToolDefinitions caps len(GenerateOptions.Tools). Excess
	// tools are dropped from the tail (with telemetry.Warn).
	MaxToolDefinitions int `json:"max_tool_definitions,omitempty" yaml:"max_tool_definitions,omitempty"`

	// MaxContextTokens is informational only — there is no built-in
	// tokenizer to enforce it. Pod-side or app-side budget probes can
	// read this via the catalog (ModelInfo.Spec.Limits) to refuse
	// oversized requests preemptively.
	MaxContextTokens int64 `json:"max_context_tokens,omitempty" yaml:"max_context_tokens,omitempty"`
}

// IsZero reports whether all limit fields are zero ("no limits").
func (l ModelLimits) IsZero() bool {
	return l.MaxOutputTokens == 0 && l.MaxToolDefinitions == 0 && l.MaxContextTokens == 0
}

// mergeSpec merges spec layers in order: later layers override earlier
// non-zero fields. Caps is OR-merged via mergeCaps (any layer disabling
// a capability wins); Defaults is field-wise overlaid via mergeOptions
// (later non-nil wins); Limits is field-wise replaced (later non-zero
// wins) with min() semantics on shared fields when both are non-zero —
// the *stricter* limit wins so deployment overrides cannot loosen what
// the catalog declared.
//
// nil / zero input layers are skipped without allocation.
func mergeSpec(layers ...ModelSpec) ModelSpec {
	var out ModelSpec
	capsLayers := make([]ModelCaps, 0, len(layers))
	for _, l := range layers {
		capsLayers = append(capsLayers, l.Caps)
		out.Defaults = mergeOptions(out.Defaults, l.Defaults)
		out.Limits = mergeLimits(out.Limits, l.Limits)
	}
	out.Caps = mergeCaps(capsLayers...)
	return out
}

// mergeLimits returns base ⊕ overlay, with the *stricter* (smaller
// non-zero) value winning on each field. Zero in either input means
// "this layer has no opinion" → take the other.
//
// Stricter-wins matters because catalog declares the model's true
// upper bound, and deployment overrides should only tighten — never
// loosen — that bound. If a deployment ever needs to bypass the
// catalog limit, it must edit the catalog registration, not patch
// it via SpecOverride.
func mergeLimits(base, overlay ModelLimits) ModelLimits {
	return ModelLimits{
		MaxOutputTokens:    minNonZero(base.MaxOutputTokens, overlay.MaxOutputTokens),
		MaxToolDefinitions: minNonZeroInt(base.MaxToolDefinitions, overlay.MaxToolDefinitions),
		MaxContextTokens:   minNonZero(base.MaxContextTokens, overlay.MaxContextTokens),
	}
}

func minNonZero(a, b int64) int64 {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

func minNonZeroInt(a, b int) int {
	switch {
	case a == 0:
		return b
	case b == 0:
		return a
	case a < b:
		return a
	default:
		return b
	}
}

// mergeOptions returns base with non-nil / non-empty fields from
// overlay copied in. base's set fields are preserved when overlay
// leaves the field unset. Used both by mergeSpec (catalog ⊕ overrides)
// and by WithDefaults (defaults ⊕ caller opts → caller wins, so it's
// called as mergeOptions(defaults, callerOpts)).
//
// Slice / map fields (StopWords, Tools, Extra) follow the "any non-nil
// overlay wins" rule (full replacement, not element-wise merge) —
// element-wise merge of these would be surprising and is rarely
// what callers want. Extra map is merged key-wise as a special case
// because that matches its role as a free-form bag.
func mergeOptions(base, overlay GenerateOptions) GenerateOptions {
	out := base

	if overlay.Temperature != nil {
		out.Temperature = overlay.Temperature
	}
	if overlay.MaxTokens != nil {
		out.MaxTokens = overlay.MaxTokens
	}
	if overlay.TopP != nil {
		out.TopP = overlay.TopP
	}
	if overlay.TopK != nil {
		out.TopK = overlay.TopK
	}
	if len(overlay.StopWords) > 0 {
		out.StopWords = append([]string(nil), overlay.StopWords...)
	}
	if overlay.FrequencyPenalty != nil {
		out.FrequencyPenalty = overlay.FrequencyPenalty
	}
	if overlay.PresencePenalty != nil {
		out.PresencePenalty = overlay.PresencePenalty
	}
	if overlay.JSONMode != nil {
		out.JSONMode = overlay.JSONMode
	}
	if overlay.JSONSchema != nil {
		out.JSONSchema = overlay.JSONSchema
	}
	if len(overlay.Tools) > 0 {
		out.Tools = append([]ToolDefinition(nil), overlay.Tools...)
	}
	if overlay.ToolChoice != nil {
		out.ToolChoice = overlay.ToolChoice
	}
	if overlay.Thinking != nil {
		out.Thinking = overlay.Thinking
	}
	if len(overlay.Extra) > 0 {
		if out.Extra == nil {
			out.Extra = make(map[string]any, len(overlay.Extra))
		}
		maps.Copy(out.Extra, overlay.Extra)
	}

	return out
}

// mergeCaps OR-merges any number of ModelCaps. A capability is
// disabled in the result if any input has it disabled. Zero-value
// inputs are skipped without allocating.
//
// OR semantics is the right default for caps because every layer
// that disables a capability is asserting "the model cannot do this
// here" — additive prohibition, never additive permission. There is
// no "force-enable" override; if a layer needs a capability disabled
// only for itself, it must not declare a global cap.
func mergeCaps(layers ...ModelCaps) ModelCaps {
	total := 0
	for _, l := range layers {
		total += len(l.Disabled)
	}
	if total == 0 {
		return ModelCaps{}
	}
	merged := ModelCaps{Disabled: make(map[Capability]bool, total)}
	for _, l := range layers {
		for k, v := range l.Disabled {
			if v {
				merged.Disabled[k] = true
			}
		}
	}
	return merged
}

// isZeroOptions reports whether o has no fields set. Used by
// ModelSpec.IsZero so the resolver can skip the WithDefaults wrap
// when defaults are empty.
func isZeroOptions(o GenerateOptions) bool {
	return o.Temperature == nil &&
		o.MaxTokens == nil &&
		o.TopP == nil &&
		o.TopK == nil &&
		len(o.StopWords) == 0 &&
		o.FrequencyPenalty == nil &&
		o.PresencePenalty == nil &&
		o.JSONMode == nil &&
		o.JSONSchema == nil &&
		len(o.Tools) == 0 &&
		o.ToolChoice == nil &&
		o.Thinking == nil &&
		len(o.Extra) == 0
}
