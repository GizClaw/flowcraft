package tts

import "context"

// Warmer is an optional interface for TTS providers that support connection warmup.
type Warmer interface {
	Warmup(ctx context.Context) error
}

// WarmupTTS pre-heats the TTS service connection pool.
// If the TTS does not implement Warmer, this is a no-op.
func WarmupTTS(ctx context.Context, t TTS) error {
	if w, ok := t.(Warmer); ok {
		return w.Warmup(ctx)
	}
	return nil
}
