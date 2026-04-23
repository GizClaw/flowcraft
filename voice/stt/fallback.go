package stt

import (
	"context"

	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/provider"
)

// FallbackSTT tries providers in order until one succeeds.
// Streaming fallback only applies when stream setup fails before a stream is returned.
type FallbackSTT struct {
	providers []STT
	policy    provider.FallbackPolicy
	circuit   *provider.Circuit
}

func NewFallbackSTT(primary STT, fallbacks ...STT) *FallbackSTT {
	return NewFallbackSTTWithPolicy(provider.DefaultFallbackPolicy(), primary, fallbacks...)
}

func NewFallbackSTTWithPolicy(policy provider.FallbackPolicy, primary STT, fallbacks ...STT) *FallbackSTT {
	providers := make([]STT, 0, 1+len(fallbacks))
	if primary != nil {
		providers = append(providers, primary)
	}
	for _, fb := range fallbacks {
		if fb != nil {
			providers = append(providers, fb)
		}
	}
	return &FallbackSTT{
		providers: providers,
		policy:    policy.Normalize(),
		circuit:   provider.NewCircuit(len(providers)),
	}
}

func (f *FallbackSTT) providerName(i int) string {
	return provider.ProviderName(f.providers[i])
}

func (f *FallbackSTT) Recognize(ctx context.Context, input audio.Frame, opts ...STTOption) (STTResult, error) {
	return provider.RunWithFallback(ctx, f.circuit, f.policy, "stt.recognize",
		len(f.providers), f.providerName,
		func(ctx context.Context, i int) (STTResult, error) {
			return f.providers[i].Recognize(ctx, input, opts...)
		},
	)
}

func (f *FallbackSTT) RecognizeStream(ctx context.Context, input audio.Stream[audio.Frame], opts ...STTOption) (audio.Stream[STTResult], error) {
	return provider.RunWithFallback(ctx, f.circuit, f.policy, "stt.recognize_stream",
		len(f.providers), f.providerName,
		func(ctx context.Context, i int) (audio.Stream[STTResult], error) {
			streamer, ok := f.providers[i].(StreamSTT)
			if !ok {
				return nil, provider.ErrSkipProvider
			}
			return streamer.RecognizeStream(ctx, input, opts...)
		},
	)
}
