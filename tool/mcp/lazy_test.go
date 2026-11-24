package mcp

import (
	"context"
	"strings"
	"testing"
	"time"
)

func lazySpec() Spec {
	return Spec{Servers: []ServerSpec{{
		Name:        "gh",
		Transport:   TransportStdio,
		Command:     "/nonexistent/definitely-not-a-binary",
		Description: "GitHub issues, pull requests and repositories.",
	}}}
}

// A declared server costs nothing until it is loaded: construction validates the
// spec and publishes a descriptor, and no process is started.
func TestNewLazyPublishesOneDescriptorPerServer(t *testing.T) {
	source, err := NewLazy(lazySpec())
	if err != nil {
		t.Fatalf("NewLazy: %v", err)
	}
	t.Cleanup(func() { _ = source.Close() })

	if got := source.Tools(); len(got) != 0 {
		t.Fatalf("tools = %d, want none before the server starts", len(got))
	}
	descriptors := source.LazyTools()
	if len(descriptors) != 1 {
		t.Fatalf("lazy tools = %d, want one per server", len(descriptors))
	}
	descriptor := descriptors[0]
	if descriptor.Name != LazyToolName("gh") {
		t.Errorf("name = %q, want %q", descriptor.Name, LazyToolName("gh"))
	}
	// The description is the only thing search can rank on before the server is
	// connected, so it must survive into the placeholder.
	if !strings.Contains(descriptor.Placeholder.Description, "GitHub issues") {
		t.Errorf("description = %q, want the configured text", descriptor.Placeholder.Description)
	}
	if descriptor.Placeholder.Name != descriptor.Name {
		t.Errorf("placeholder name = %q, want %q", descriptor.Placeholder.Name, descriptor.Name)
	}
}

// A server that cannot start fails where the model asked for it, not at
// assembly: that is the whole point of deferring.
func TestLazyLoadFailsOnUseNotAtConstruction(t *testing.T) {
	source, err := NewLazy(lazySpec())
	if err != nil {
		t.Fatalf("NewLazy: %v", err)
	}
	source.SetConnectTimeout(50 * time.Millisecond)
	t.Cleanup(func() { _ = source.Close() })

	descriptor := source.LazyTools()[0]
	if _, err := descriptor.Load(context.Background()); err == nil {
		t.Fatal("loading a server whose command does not exist succeeded")
	}
}
