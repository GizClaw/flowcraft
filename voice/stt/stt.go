package stt

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/voice/audio"
)

// STT is the one-shot speech recognition interface.
type STT interface {
	Recognize(ctx context.Context, input audio.Frame, opts ...STTOption) (STTResult, error)
}

// StreamSTT extends STT with streaming recognition.
// Both input and output use Stream[T] so that providers can distinguish
// normal end (io.EOF) from interruption (e.g. barge-in).
type StreamSTT interface {
	STT
	RecognizeStream(ctx context.Context, input audio.Stream[audio.Frame], opts ...STTOption) (audio.Stream[STTResult], error)
}

// STTResult is a recognition result fragment.
type STTResult struct {
	Text       string
	Audio      audio.Frame
	IsFinal    bool
	Lang       string
	Confidence float64
	Duration   time.Duration
	Words      []WordTiming
}

// WordTiming contains timing information for a single word.
type WordTiming struct {
	Word  string
	Start time.Duration
	End   time.Duration
}

// ---------------------------------------------------------------------------
// STT Provider Registry
// ---------------------------------------------------------------------------

// STTProviderFactory creates an STT instance.
type STTProviderFactory func(apiKey, baseURL string, opts ...STTProviderOption) (STT, error)

// STTProviderOption configures a provider at creation time.
type STTProviderOption interface {
	ApplySTTProvider(target any)
}

// STTProviderOptionFunc wraps a function as an STTProviderOption.
type STTProviderOptionFunc func(target any)

func (f STTProviderOptionFunc) ApplySTTProvider(target any) { f(target) }

// STTRegistry is a thread-safe registry of STT provider factories.
type STTRegistry struct {
	mu        sync.RWMutex
	providers map[string]STTProviderFactory
}

func NewSTTRegistry() *STTRegistry {
	return &STTRegistry{providers: make(map[string]STTProviderFactory)}
}

func (r *STTRegistry) Register(name string, factory STTProviderFactory) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.providers[name] = factory
}

func (r *STTRegistry) New(provider, apiKey, baseURL string, opts ...STTProviderOption) (STT, error) {
	r.mu.RLock()
	factory, ok := r.providers[provider]
	r.mu.RUnlock()
	if !ok {
		return nil, errdefs.NotFoundf("stt_provider %q not found", provider)
	}
	return factory(apiKey, baseURL, opts...)
}

func (r *STTRegistry) ListProviders() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.providers))
	for n := range r.providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var DefaultSTTRegistry = NewSTTRegistry()

func RegisterSTT(name string, factory STTProviderFactory) {
	DefaultSTTRegistry.Register(name, factory)
}

func NewSTT(provider, apiKey, baseURL string, opts ...STTProviderOption) (STT, error) {
	return DefaultSTTRegistry.New(provider, apiKey, baseURL, opts...)
}

func ListSTTProviders() []string { return DefaultSTTRegistry.ListProviders() }
