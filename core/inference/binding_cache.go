package inference

import (
	"context"
	"errors"
	"sync"

	"github.com/GizClaw/flowcraft/core/telemetry"

	otellog "go.opentelemetry.io/otel/log"
)

// maxCachedBindings bounds a BindingCache. Hosts that address models from
// dynamic input (a graph config derived from the board, a script argument) can
// name many models over a process's life; the bound keeps that from growing the
// map without limit. Eviction is least-recently-used, so the models a host
// keeps addressing stay open while one-off targets cycle out. A model that was
// evicted and is addressed again is opened afresh, which also re-resolves its
// credentials.
const maxCachedBindings = 16

// bindingEntry is one in-flight open. Callers that reach the same new model
// share the entry, so a burst opens it once.
type bindingEntry struct {
	ready   chan struct{}
	binding *Binding
	err     error
}

// cachedBinding is one retained open plus its recency stamp.
type cachedBinding struct {
	binding *Binding
	used    uint64
}

// BindingCache is the host side of Binding: it opens each model reference once
// and reuses the handle, keyed by the full reference (provider, model,
// credential profile).
//
// It exists because opening is deployment-scoped work while a host's calls are
// not: a graph node or a script runtime is long-lived and addresses a small set
// of models, and rebuilding the provider's clients on every call would be
// wasted work and a fresh connection each time. The cache is opt-in — the
// Assembly's own call path still resolves per call — so a host that wants the
// reuse says so by holding a BindingCache.
//
// A cache is safe for concurrent use. Failures are never cached: a credential
// configured after the first attempt must succeed on the next one. The
// staleness window for a model that stays cached is the host's lifetime: a
// rotated credential or client setting takes effect when the model is evicted
// and reopened, or when the host is rebuilt. Every open is logged with
// llm.provider, llm.model, and llm.profile so that window is visible.
type BindingCache struct {
	assembly *Assembly

	mu      sync.Mutex
	clock   uint64
	cached  map[ModelRef]*cachedBinding
	pending map[ModelRef]*bindingEntry
}

// NewBindingCache returns an empty cache over assembly.
func NewBindingCache(assembly *Assembly) *BindingCache {
	return &BindingCache{
		assembly: assembly,
		cached:   make(map[ModelRef]*cachedBinding),
		pending:  make(map[ModelRef]*bindingEntry),
	}
}

// Bind returns the cached binding for ref, opening it on first use. Opening
// runs outside the cache lock, so a slow first open blocks neither cache hits
// for other models nor their first opens.
func (c *BindingCache) Bind(ctx context.Context, ref ModelRef) (*Binding, error) {
	if c == nil || c.assembly == nil {
		return nil, NewError(
			InvalidRequest, "", "",
			errors.New("binding cache has no assembly"),
		)
	}
	c.mu.Lock()
	if entry, ok := c.cached[ref]; ok {
		entry.used = c.next()
		binding := entry.binding
		c.mu.Unlock()
		return binding, nil
	}
	if entry, ok := c.pending[ref]; ok {
		c.mu.Unlock()
		<-entry.ready
		return entry.binding, entry.err
	}
	entry := &bindingEntry{ready: make(chan struct{})}
	c.pending[ref] = entry
	c.mu.Unlock()

	binding, err := c.open(ctx, ref)

	c.mu.Lock()
	entry.binding, entry.err = binding, err
	delete(c.pending, ref)
	if err == nil {
		c.store(ref, binding)
	}
	close(entry.ready)
	c.mu.Unlock()
	return binding, err
}

// open resolves one model reference and records the event.
func (c *BindingCache) open(ctx context.Context, ref ModelRef) (*Binding, error) {
	binding, err := c.assembly.Bind(ctx, ref)
	if err != nil {
		return nil, err
	}
	telemetry.Debug(ctx, "inference: opened model drivers",
		otellog.String(telemetry.AttrLLMProvider, ref.ID.Provider),
		otellog.String(telemetry.AttrLLMModel, ref.ID.Name),
		otellog.String("llm.profile", ref.Profile))
	return binding, nil
}

// store retains one binding, evicting the least recently used entry when the
// cache is at capacity. Eviction only drops the reference: a call already
// executing against that binding keeps its drivers until it finishes. Callers
// must hold c.mu.
func (c *BindingCache) store(ref ModelRef, binding *Binding) {
	if _, exists := c.cached[ref]; !exists && len(c.cached) >= maxCachedBindings {
		var (
			oldest     ModelRef
			oldestUsed uint64
			found      bool
		)
		for key, entry := range c.cached {
			if !found || entry.used < oldestUsed {
				oldest, oldestUsed, found = key, entry.used, true
			}
		}
		delete(c.cached, oldest)
	}
	c.cached[ref] = &cachedBinding{binding: binding, used: c.next()}
}

// next advances the recency clock. Callers must hold c.mu.
func (c *BindingCache) next() uint64 {
	c.clock++
	return c.clock
}

// PrepareGenerate compiles one unary generate request against the cached
// binding of ref.
func (c *BindingCache) PrepareGenerate(
	ctx context.Context,
	ref ModelRef,
	request GenerateRequest,
) (*Prepared[GenerateResponse], error) {
	binding, err := c.Bind(ctx, ref)
	if err != nil {
		return nil, err
	}
	return binding.PrepareGenerate(ctx, request)
}

// PrepareGenerateStream compiles one streaming generate request against the
// cached binding of ref.
func (c *BindingCache) PrepareGenerateStream(
	ctx context.Context,
	ref ModelRef,
	request GenerateRequest,
) (*Prepared[GenerateStream], error) {
	binding, err := c.Bind(ctx, ref)
	if err != nil {
		return nil, err
	}
	return binding.PrepareGenerateStream(ctx, request)
}

// PrepareEmbed compiles one embedding request against the cached binding of
// ref.
func (c *BindingCache) PrepareEmbed(
	ctx context.Context,
	ref ModelRef,
	request EmbedRequest,
) (*Prepared[EmbedResponse], error) {
	binding, err := c.Bind(ctx, ref)
	if err != nil {
		return nil, err
	}
	return binding.PrepareEmbed(ctx, request)
}

// PrepareTranscribe compiles one whole-file transcription request against the
// cached binding of ref.
func (c *BindingCache) PrepareTranscribe(
	ctx context.Context,
	ref ModelRef,
	request TranscriptionRequest,
) (*Prepared[TranscriptionResponse], error) {
	binding, err := c.Bind(ctx, ref)
	if err != nil {
		return nil, err
	}
	return binding.PrepareTranscribe(ctx, request)
}

// PrepareTranscribeSession compiles one duplex transcription session request
// against the cached binding of ref.
func (c *BindingCache) PrepareTranscribeSession(
	ctx context.Context,
	ref ModelRef,
	request TranscriptionSessionRequest,
) (*Prepared[TranscriptionSession], error) {
	binding, err := c.Bind(ctx, ref)
	if err != nil {
		return nil, err
	}
	return binding.PrepareTranscribeSession(ctx, request)
}
