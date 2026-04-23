package tts

import "github.com/GizClaw/flowcraft/voice/audio"

// TTSOption configures a single Synthesize/SynthesizeStream call.
type TTSOption func(*TTSOptions)

// TTSOptions holds TTS call parameters.
type TTSOptions struct {
	Voice string
	Speed float64
	Codec audio.Codec
	Rate  int

	// Extra holds provider-specific parameters keyed by namespaced strings
	// (e.g. "minimax.emotion", "azure.style"). Providers read from this map
	// in their buildRequest path, falling back to instance-level defaults.
	Extra map[string]any
}

// ApplyTTSOptions folds option funcs into a TTSOptions value.
func ApplyTTSOptions(opts ...TTSOption) *TTSOptions {
	o := &TTSOptions{Speed: 1.0, Codec: audio.CodecPCM, Rate: 24000}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

func WithVoice(voice string) TTSOption  { return func(o *TTSOptions) { o.Voice = voice } }
func WithSpeed(speed float64) TTSOption { return func(o *TTSOptions) { o.Speed = speed } }
func WithCodec(c audio.Codec) TTSOption { return func(o *TTSOptions) { o.Codec = c } }
func WithRate(rate int) TTSOption       { return func(o *TTSOptions) { o.Rate = rate } }

// WithExtra sets a provider-specific parameter. Providers define typed
// convenience wrappers (e.g. minimax.WithEmotion) that delegate here.
func WithExtra(key string, value any) TTSOption {
	return func(o *TTSOptions) {
		if o.Extra == nil {
			o.Extra = make(map[string]any)
		}
		o.Extra[key] = value
	}
}

// ExtraString reads a string from Extra, returning fallback if absent or wrong type.
func (o *TTSOptions) ExtraString(key, fallback string) string {
	if o.Extra == nil {
		return fallback
	}
	if v, ok := o.Extra[key].(string); ok {
		return v
	}
	return fallback
}

// ExtraFloat64 reads a float64 from Extra, returning fallback if absent or wrong type.
func (o *TTSOptions) ExtraFloat64(key string, fallback float64) float64 {
	if o.Extra == nil {
		return fallback
	}
	if v, ok := o.Extra[key].(float64); ok {
		return v
	}
	return fallback
}

// ExtraInt reads an int from Extra, returning fallback if absent or wrong type.
func (o *TTSOptions) ExtraInt(key string, fallback int) int {
	if o.Extra == nil {
		return fallback
	}
	if v, ok := o.Extra[key].(int); ok {
		return v
	}
	return fallback
}
