package dynamic

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	sdktool "github.com/GizClaw/flowcraft/sdk/tool"
	"golang.org/x/sync/singleflight"
)

// Loader loads the real implementation of a deferred tool. It is the
// only place a LazyTool may do network or process work; Definition()
// and Get() never call it.
type Loader func(ctx context.Context) (sdktool.Tool, error)

// RetryPolicy bounds how many times EnsureLoaded attempts the loader.
type RetryPolicy struct {
	// Attempts is the total number of loader invocations, including the
	// first. Zero falls back to DefaultRetryPolicy.Attempts.
	Attempts int
	// BaseDelay is the first retry delay; each retry doubles it.
	BaseDelay time.Duration
	// MaxDelay caps the exponential backoff.
	MaxDelay time.Duration
}

// DefaultRetryPolicy is the built-in retry policy: three attempts with
// 100ms, 200ms, 400ms backoff (capped at 1s).
var DefaultRetryPolicy = RetryPolicy{
	Attempts:  3,
	BaseDelay: 100 * time.Millisecond,
	MaxDelay:  time.Second,
}

func (p RetryPolicy) attempts() int {
	if p.Attempts <= 0 {
		return DefaultRetryPolicy.Attempts
	}
	return p.Attempts
}

// LazyTool is a stable registry entry for a deferred tool. Before it is
// loaded it serves a placeholder definition; after EnsureLoaded it
// forwards to the real tool — preferring the registry's current entry
// for that name so a later MCP reconcile is picked up automatically.
type LazyTool struct {
	name        string
	reg         *sdktool.Registry
	loader      Loader
	placeholder message.Definition
	retry       RetryPolicy

	group   singleflight.Group
	mu      sync.RWMutex
	inner   sdktool.Tool
	loaded  bool
	lastErr error
	closed  bool
}

var (
	_ sdktool.Tool         = (*LazyTool)(nil)
	_ sdktool.ToolMetadata = (*LazyTool)(nil)
	_ io.Closer            = (*LazyTool)(nil)
)

// NewLazyTool creates a deferred tool proxy. reg is the registry the
// proxy is registered into (optional); when set, forwarding prefers the
// registry's current entry so external reconciles stay visible.
func NewLazyTool(reg *sdktool.Registry, name string, loader Loader, opts ...LazyOption) *LazyTool {
	if name == "" {
		panic("dynamic.NewLazyTool: name is empty")
	}
	if loader == nil {
		panic("dynamic.NewLazyTool: loader is nil")
	}
	t := &LazyTool{
		name:   name,
		reg:    reg,
		loader: loader,
		placeholder: message.Definition{
			Name:        name,
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		retry: DefaultRetryPolicy,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(t)
		}
	}
	if t.placeholder.Name == "" {
		t.placeholder.Name = name
	}
	if len(t.placeholder.InputSchema) == 0 {
		t.placeholder.InputSchema = json.RawMessage(`{"type":"object"}`)
	}
	return t
}

// LazyOption configures a LazyTool.
type LazyOption func(*LazyTool)

// WithPlaceholder overrides the definition served before loading. The
// name is forced to the proxy's name so the registry key stays stable.
func WithPlaceholder(def message.Definition) LazyOption {
	return func(t *LazyTool) {
		def.Name = t.name
		t.placeholder = def
	}
}

// WithRetryPolicy overrides the loader retry policy.
func WithRetryPolicy(p RetryPolicy) LazyOption {
	return func(t *LazyTool) { t.retry = p }
}

// EnsureLoaded loads the real tool at most once concurrently
// (singleflight) and retries on failure per the retry policy. A failed
// load keeps the proxy in the unloaded state so the next call may
// retry; a closed proxy never loads again.
func (t *LazyTool) EnsureLoaded(ctx context.Context) error {
	if ctx == nil {
		return errdefs.Validationf("dynamic: EnsureLoaded: context is nil")
	}
	if t.isClosed() {
		return errdefs.NotAvailablef("dynamic: tool %q is closed", t.name)
	}
	_, err, _ := t.group.Do(t.name, func() (any, error) {
		return nil, t.loadOnce(ctx)
	})
	return err
}

func (t *LazyTool) loadOnce(ctx context.Context) error {
	t.mu.RLock()
	loaded := t.loaded
	t.mu.RUnlock()
	if loaded {
		return nil
	}

	var lastErr error
	attempts := t.retry.attempts()
	for attempt := range attempts {
		if attempt > 0 {
			delay := backoff(t.retry.BaseDelay, t.retry.MaxDelay, attempt-1)
			if err := sleepCtx(ctx, delay); err != nil {
				return err
			}
		}
		tool, err := t.loader(ctx)
		if err == nil && tool == nil {
			err = errdefs.Internalf("dynamic: loader for %q returned a nil tool", t.name)
		}
		if err == nil {
			t.mu.Lock()
			if !t.closed {
				t.inner = tool
				t.loaded = true
				t.lastErr = nil
			}
			t.mu.Unlock()
			return nil
		}
		lastErr = err
	}
	t.mu.Lock()
	if !t.closed {
		t.lastErr = lastErr
	}
	t.mu.Unlock()
	return lastErr
}

// Definition returns the loaded tool's definition, or the placeholder
// while unloaded. It never loads and never fails.
func (t *LazyTool) Definition() message.Definition {
	if current := t.current(); current != nil {
		return current.Definition()
	}
	return t.placeholder
}

// Execute loads the real tool (with retries) and forwards the call. A
// failed or closed load surfaces as a typed NotAvailable error.
func (t *LazyTool) Execute(ctx context.Context, arguments string) (string, error) {
	if err := t.EnsureLoaded(ctx); err != nil {
		return "", errdefs.NotAvailablef(
			"dynamic: tool %q is not available: %v", t.name, err)
	}
	current := t.current()
	if current == nil {
		return "", errdefs.NotAvailablef(
			"dynamic: tool %q loaded without an implementation", t.name)
	}
	return current.Execute(ctx, arguments)
}

// Metadata reports the loaded tool's metadata, or a conservative zero
// ToolMeta while unloaded.
func (t *LazyTool) Metadata() sdktool.ToolMeta {
	if current := t.current(); current != nil {
		return sdktool.MetadataOf(current)
	}
	return sdktool.ToolMeta{}
}

// Close idempotently closes the loaded implementation (if it is an
// io.Closer) and forbids further loads.
func (t *LazyTool) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	inner := t.inner
	t.inner = nil
	t.mu.Unlock()
	if closer, ok := inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// LastError returns the most recent load failure, or nil when the
// proxy is loaded or never attempted.
func (t *LazyTool) LastError() error {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.lastErr
}

func (t *LazyTool) isClosed() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.closed
}

// current prefers the registry's live entry (it may have been replaced
// by a reconcile) and falls back to the loaded inner tool. Until the
// proxy is loaded it never consults the registry: Registry.Register
// calls Definition() while holding its write lock, and consulting the
// registry there would self-deadlock.
func (t *LazyTool) current() sdktool.Tool {
	t.mu.RLock()
	loaded := t.loaded
	inner := t.inner
	t.mu.RUnlock()
	if !loaded {
		return nil
	}
	if t.reg != nil {
		if candidate, ok := t.reg.Get(t.name); ok && candidate != t {
			return candidate
		}
	}
	return inner
}

func backoff(base, max time.Duration, attempt int) time.Duration {
	if base <= 0 {
		return 0
	}
	delay := base
	for i := 0; i < attempt && delay < max; i++ {
		delay *= 2
	}
	if max > 0 && delay > max {
		return max
	}
	return delay
}

func sleepCtx(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
