package llm

import "context"

// WithDefaults wraps inner so that GenerateOptions fields the caller
// did not set are filled from the supplied defaults. Caller-set
// fields always win; the middleware only fills nil pointers and
// empty slices in the per-call options.
//
// If defaults is the zero value (no fields set), inner is returned
// unwrapped — the resolver relies on this to skip the wrap entirely
// when ModelSpec.Defaults is empty.
func WithDefaults(inner LLM, defaults GenerateOptions) LLM {
	if isZeroOptions(defaults) {
		return inner
	}
	return &defaultsLLM{inner: inner, defaults: defaults}
}

type defaultsLLM struct {
	inner    LLM
	defaults GenerateOptions
}

func (d *defaultsLLM) Generate(ctx context.Context, msgs []Message, opts ...GenerateOption) (Message, TokenUsage, error) {
	return d.inner.Generate(ctx, msgs, d.fill(opts)...)
}

func (d *defaultsLLM) GenerateStream(ctx context.Context, msgs []Message, opts ...GenerateOption) (StreamMessage, error) {
	return d.inner.GenerateStream(ctx, msgs, d.fill(opts)...)
}

// fill prepends a synthetic option that overlays defaults into any
// field the caller's opts left unset. We prepend (rather than write
// directly to a *GenerateOptions) so caller-supplied opts run last
// and can still override every default — keeping the "caller wins"
// invariant from §3.1 of the design RFC.
func (d *defaultsLLM) fill(opts []GenerateOption) []GenerateOption {
	if len(opts) == 0 {
		// No caller opts at all — emit one option that just installs
		// the defaults verbatim.
		return []GenerateOption{applyDefaults(d.defaults)}
	}
	// Apply caller opts to a temp options value first to discover
	// which fields the caller actually set; then build a final
	// GenerateOption that fills only the still-unset fields.
	out := make([]GenerateOption, 0, len(opts)+1)
	out = append(out, opts...)
	out = append(out, fillUnset(d.defaults))
	return out
}

// applyDefaults is the no-caller-opts fast path: copy defaults into
// the GenerateOptions value verbatim. Slice / map fields are
// shallow-copied to keep the defaults value immutable.
func applyDefaults(defaults GenerateOptions) GenerateOption {
	return func(o *GenerateOptions) {
		*o = mergeOptions(*o, defaults)
	}
}

// fillUnset returns an option that, when applied LAST, fills only
// the fields the prior options left at zero / nil. This is the
// "caller wins" path: applied after the caller's WithTemperature
// etc., it inspects what's already there and only completes the gaps.
func fillUnset(defaults GenerateOptions) GenerateOption {
	return func(o *GenerateOptions) {
		if o.Temperature == nil && defaults.Temperature != nil {
			t := *defaults.Temperature
			o.Temperature = &t
		}
		if o.MaxTokens == nil && defaults.MaxTokens != nil {
			m := *defaults.MaxTokens
			o.MaxTokens = &m
		}
		if o.TopP == nil && defaults.TopP != nil {
			p := *defaults.TopP
			o.TopP = &p
		}
		if o.TopK == nil && defaults.TopK != nil {
			k := *defaults.TopK
			o.TopK = &k
		}
		if len(o.StopWords) == 0 && len(defaults.StopWords) > 0 {
			o.StopWords = append([]string(nil), defaults.StopWords...)
		}
		if o.FrequencyPenalty == nil && defaults.FrequencyPenalty != nil {
			f := *defaults.FrequencyPenalty
			o.FrequencyPenalty = &f
		}
		if o.PresencePenalty == nil && defaults.PresencePenalty != nil {
			p := *defaults.PresencePenalty
			o.PresencePenalty = &p
		}
		if o.JSONMode == nil && defaults.JSONMode != nil {
			m := *defaults.JSONMode
			o.JSONMode = &m
		}
		if o.JSONSchema == nil && defaults.JSONSchema != nil {
			s := *defaults.JSONSchema
			o.JSONSchema = &s
		}
		if len(o.Tools) == 0 && len(defaults.Tools) > 0 {
			o.Tools = append([]ToolDefinition(nil), defaults.Tools...)
		}
		if o.ToolChoice == nil && defaults.ToolChoice != nil {
			c := *defaults.ToolChoice
			o.ToolChoice = &c
		}
		if o.Thinking == nil && defaults.Thinking != nil {
			th := *defaults.Thinking
			o.Thinking = &th
		}
		if len(defaults.Extra) > 0 {
			if o.Extra == nil {
				o.Extra = make(map[string]any, len(defaults.Extra))
			}
			for k, v := range defaults.Extra {
				if _, present := o.Extra[k]; !present {
					o.Extra[k] = v
				}
			}
		}
	}
}
