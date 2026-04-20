package llm

import "context"

// CapsMiddleware wraps an LLM and filters/downgrades unsupported params
// based on ModelCaps before forwarding to the inner LLM.
// If caps is zero-value (all features supported), returns inner as-is.
func CapsMiddleware(inner LLM, caps ModelCaps) LLM {
	if caps.IsZero() {
		return inner
	}
	return &capsLLM{inner: inner, caps: caps}
}

type capsLLM struct {
	inner LLM
	caps  ModelCaps
}

func (c *capsLLM) Generate(ctx context.Context, msgs []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	return c.inner.Generate(ctx, msgs, c.filtered(opts)...)
}

func (c *capsLLM) GenerateStream(ctx context.Context, msgs []Message, opts ...GenerateOption) (StreamMessage, error) {
	return c.inner.GenerateStream(ctx, msgs, c.filtered(opts)...)
}

func (c *capsLLM) filtered(opts []GenerateOption) []GenerateOption {
	result := make([]GenerateOption, len(opts))
	copy(result, opts)
	if !c.caps.Supports(CapTemperature) {
		result = append(result, clearTemperature)
	}
	if !c.caps.Supports(CapJSONSchema) {
		result = append(result, downgradeJSONSchema)
	}
	if !c.caps.Supports(CapJSONMode) {
		result = append(result, clearJSONMode)
	}
	return result
}

var clearTemperature GenerateOption = func(o *GenerateOptions) { o.Temperature = nil }
var clearJSONMode GenerateOption = func(o *GenerateOptions) { o.JSONMode = nil }
var downgradeJSONSchema GenerateOption = func(o *GenerateOptions) {
	if o.JSONSchema != nil {
		o.JSONSchema = nil
		t := true
		o.JSONMode = &t
	}
}

// mergeCaps OR-merges any number of ModelCaps. A capability is
// disabled in the result if any input has it disabled. Zero-value
// inputs are skipped without allocating.
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

// unwrapCaps returns the inner LLM if l was wrapped by CapsMiddleware,
// otherwise l itself. Used by the resolver to avoid double-wrapping
// when re-applying merged caps on top of the registry's already-wrapped
// instance.
func unwrapCaps(l LLM) LLM {
	if c, ok := l.(*capsLLM); ok {
		return c.inner
	}
	return l
}
