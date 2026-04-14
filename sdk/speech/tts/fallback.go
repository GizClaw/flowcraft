package tts

import (
	"context"
	"io"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/provider"
)

// FallbackTTS tries providers in order until one succeeds.
// Streaming fallback only applies when stream setup fails before a stream is returned.
type FallbackTTS struct {
	providers []TTS
	policy    provider.FallbackPolicy
	circuit   *provider.Circuit
}

func NewFallbackTTS(primary TTS, fallbacks ...TTS) *FallbackTTS {
	return NewFallbackTTSWithPolicy(provider.DefaultFallbackPolicy(), primary, fallbacks...)
}

func NewFallbackTTSWithPolicy(policy provider.FallbackPolicy, primary TTS, fallbacks ...TTS) *FallbackTTS {
	providers := make([]TTS, 0, 1+len(fallbacks))
	if primary != nil {
		providers = append(providers, primary)
	}
	for _, fb := range fallbacks {
		if fb != nil {
			providers = append(providers, fb)
		}
	}
	return &FallbackTTS{
		providers: providers,
		policy:    policy.Normalize(),
		circuit:   provider.NewCircuit(len(providers)),
	}
}

func (f *FallbackTTS) providerName(i int) string {
	return provider.ProviderName(f.providers[i])
}

func (f *FallbackTTS) Synthesize(ctx context.Context, text string, opts ...TTSOption) (io.ReadCloser, error) {
	return provider.RunWithFallback(ctx, f.circuit, f.policy, "tts.synthesize",
		len(f.providers), f.providerName,
		func(ctx context.Context, i int) (io.ReadCloser, error) {
			return f.providers[i].Synthesize(ctx, text, opts...)
		},
	)
}

func (f *FallbackTTS) Voices(ctx context.Context) ([]Voice, error) {
	return provider.RunWithFallback(ctx, f.circuit, f.policy, "tts.voices",
		len(f.providers), f.providerName,
		func(ctx context.Context, i int) ([]Voice, error) {
			return f.providers[i].Voices(ctx)
		},
	)
}

func (f *FallbackTTS) SynthesizeStream(ctx context.Context, input audio.Stream[string], opts ...TTSOption) (audio.Stream[Utterance], error) {
	return provider.RunWithFallback(ctx, f.circuit, f.policy, "tts.synthesize_stream",
		len(f.providers), f.providerName,
		func(ctx context.Context, i int) (audio.Stream[Utterance], error) {
			streamer, ok := f.providers[i].(StreamTTS)
			if !ok {
				return nil, provider.ErrSkipProvider
			}
			return streamer.SynthesizeStream(ctx, input, opts...)
		},
	)
}
