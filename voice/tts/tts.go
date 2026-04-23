package tts

import (
	"context"
	"io"
	"sort"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/voice/audio"
)

// Voice describes an available TTS voice.
type Voice struct {
	ID   string
	Name string
	Lang string
}

// Utterance is a fragment of synthesized speech with metadata.
// Multiple utterances from the same sentence share the same ChunkID.
type Utterance struct {
	audio.Frame        // embedded: Data + Format
	Text        string // the original text that produced this audio
	ChunkID     string // unique ID per sentence (shared across chunks of the same sentence)
	Sequence    int
}

// TTS is the speech synthesis interface.
type TTS interface {
	Synthesize(ctx context.Context, text string, opts ...TTSOption) (io.ReadCloser, error)
	Voices(ctx context.Context) ([]Voice, error)
}

// StreamTTS extends TTS with streaming synthesis.
// Input is Stream[string] (sentences from Segmenter), output is Stream[Utterance].
type StreamTTS interface {
	TTS
	SynthesizeStream(ctx context.Context, input audio.Stream[string], opts ...TTSOption) (audio.Stream[Utterance], error)
}

// ---------------------------------------------------------------------------
// TTS Provider Registry
// ---------------------------------------------------------------------------

// TTSProviderFactory creates a TTS instance.
type TTSProviderFactory func(apiKey, baseURL string, opts ...TTSProviderOption) (TTS, error)

// TTSProviderOption configures a provider at creation time.
type TTSProviderOption interface {
	ApplyTTSProvider(target any)
}

// TTSProviderOptionFunc wraps a function as a TTSProviderOption.
type TTSProviderOptionFunc func(target any)

func (f TTSProviderOptionFunc) ApplyTTSProvider(target any) { f(target) }

// TTSRegistry is a thread-safe registry of TTS provider factories.
type TTSRegistry struct {
	mu        sync.RWMutex
	providers map[string]TTSProviderFactory
}

func NewTTSRegistry() *TTSRegistry {
	return &TTSRegistry{providers: make(map[string]TTSProviderFactory)}
}

func (r *TTSRegistry) Register(name string, factory TTSProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = factory
}

func (r *TTSRegistry) New(provider, apiKey, baseURL string, opts ...TTSProviderOption) (TTS, error) {
	r.mu.RLock()
	factory, ok := r.providers[provider]
	r.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFoundf("tts_provider %q not found", provider)
	}
	return factory(apiKey, baseURL, opts...)
}

func (r *TTSRegistry) ListProviders() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var DefaultTTSRegistry = NewTTSRegistry()

func RegisterTTS(name string, factory TTSProviderFactory) {
	DefaultTTSRegistry.Register(name, factory)
}

func NewTTS(provider, apiKey, baseURL string, opts ...TTSProviderOption) (TTS, error) {
	return DefaultTTSRegistry.New(provider, apiKey, baseURL, opts...)
}

func ListTTSProviders() []string { return DefaultTTSRegistry.ListProviders() }
