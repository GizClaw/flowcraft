package engine_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
)

func TestWithHost_RoundTrip(t *testing.T) {
	want := engine.NoopHost{}
	ctx := engine.WithHost(context.Background(), want)
	got, ok := engine.HostFromContext(ctx)
	if !ok {
		t.Fatal("HostFromContext returned ok=false after WithHost")
	}
	if got != want {
		t.Errorf("HostFromContext returned %v, want %v", got, want)
	}
}

func TestWithHost_NilHostIsNoop(t *testing.T) {
	parent := context.Background()
	ctx := engine.WithHost(parent, nil)
	if ctx != parent {
		t.Errorf("WithHost(nil) must return ctx unchanged so callers can plumb unconditionally")
	}
	if _, ok := engine.HostFromContext(ctx); ok {
		t.Errorf("HostFromContext returned ok=true after WithHost(nil)")
	}
}

func TestHostFromContext_NilCtxReturnsFalse(t *testing.T) {
	if h, ok := engine.HostFromContext(nil); ok || h != nil {
		t.Errorf("nil ctx must yield (nil, false); got (%v, %v)", h, ok)
	}
}

func TestHostFromContext_BareCtxReturnsFalse(t *testing.T) {
	if _, ok := engine.HostFromContext(context.Background()); ok {
		t.Errorf("bare ctx must yield ok=false")
	}
}
