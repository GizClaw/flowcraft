package stt

// STTOption configures a single Recognize/RecognizeStream call.
type STTOption func(*STTOptions)

// STTOptions holds STT call parameters.
type STTOptions struct {
	Language         string
	TargetSampleRate int // desired sample rate for the STT provider; 0 means no resampling

	// Extra holds provider-specific parameters keyed by namespaced strings
	// (e.g. "bytedance.end_window"). Providers read from this map
	// in their request-building path, falling back to instance-level defaults.
	Extra map[string]any
}

// ApplySTTOptions folds option funcs into an STTOptions value.
func ApplySTTOptions(opts ...STTOption) *STTOptions {
	o := &STTOptions{}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

// WithLanguage sets the recognition language hint.
func WithLanguage(lang string) STTOption {
	return func(o *STTOptions) { o.Language = lang }
}

// WithTargetSampleRate tells the pipeline to resample input audio to the
// given rate before sending it to the STT provider. A value of 0 (default)
// disables resampling and sends audio as-is.
func WithTargetSampleRate(rate int) STTOption {
	return func(o *STTOptions) { o.TargetSampleRate = rate }
}

// WithExtra sets a provider-specific parameter. Providers define typed
// convenience wrappers (e.g. bytedance.EndWindow) that delegate here.
func WithExtra(key string, value any) STTOption {
	return func(o *STTOptions) {
		if o.Extra == nil {
			o.Extra = make(map[string]any)
		}
		o.Extra[key] = value
	}
}

// ExtraString reads a string from Extra, returning fallback if absent or wrong type.
func (o *STTOptions) ExtraString(key, fallback string) string {
	if o.Extra == nil {
		return fallback
	}
	if v, ok := o.Extra[key].(string); ok {
		return v
	}
	return fallback
}

// ExtraInt reads an int from Extra, returning fallback if absent or wrong type.
func (o *STTOptions) ExtraInt(key string, fallback int) int {
	if o.Extra == nil {
		return fallback
	}
	if v, ok := o.Extra[key].(int); ok {
		return v
	}
	return fallback
}

// ExtraBool reads a bool from Extra, returning fallback if absent or wrong type.
func (o *STTOptions) ExtraBool(key string, fallback bool) bool {
	if o.Extra == nil {
		return fallback
	}
	if v, ok := o.Extra[key].(bool); ok {
		return v
	}
	return fallback
}
