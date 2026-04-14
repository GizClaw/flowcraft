package webrtc

// AudioEncoder encodes PCM frames to a compressed format (e.g. Opus).
// Implementations are injected via TransportConfig to keep this package
// free of CGo dependencies.
type AudioEncoder interface {
	// Encode encodes a PCM16LE buffer into compressed audio.
	// The pcm slice contains interleaved samples at the encoder's configured
	// sample rate and channel count.
	Encode(pcm []byte) ([]byte, error)

	// Reset clears any internal state accumulated across Encode calls.
	Reset()
}

// AudioDecoder decodes compressed audio (e.g. Opus) to PCM16LE.
// Implementations are injected via TransportConfig.
type AudioDecoder interface {
	// Decode decodes a single compressed packet into PCM16LE samples.
	Decode(data []byte) ([]byte, error)

	// Reset clears any internal state accumulated across Decode calls.
	Reset()
}
