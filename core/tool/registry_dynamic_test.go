package tool_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// dynamicSource stands in for the source that publishes tools after construction: an MCP
// server that connects in the background, a refresh that re-lists remote tools.
type dynamicSource struct {
	mu       sync.Mutex
	attached tool.Registrar
	detached int
	err      error
}

func (s *dynamicSource) Tools() []tool.Tool         { return nil }
func (s *dynamicSource) LazyTools() []tool.LazyTool { return nil }

func (s *dynamicSource) Attach(r tool.Registrar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attached = r
}

func (s *dynamicSource) Detach(tool.Registrar) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.detached++
	// Dropping the registrar is the point of Detach: a source that outlives the
	// registry must not keep it, and the tools it held, reachable.
	s.attached = nil
}

// publish registers a tool the way a background list change would.
func (s *dynamicSource) publish(name string) error {
	s.mu.Lock()
	registrar, err := s.attached, s.err
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if registrar == nil {
		return errors.New("dynamicSource: not attached")
	}
	return registrar.Add(simpleTool(name))
}

func (s *dynamicSource) remove(name string) {
	s.mu.Lock()
	registrar := s.attached
	s.mu.Unlock()
	if registrar != nil {
		registrar.Remove(name)
	}
}

func simpleTool(name string) tool.Tool {
	return tool.TextTool(message.ToolDefinition{Name: name, InputSchema: []byte(`{"type":"object"}`)},
		func(_ context.Context, _ string) (string, error) { return name, nil })
}

// TestDynamicSourcePublishesIntoTherRegistry is the MCP shape: the source connects after
// the registry exists, is handed the registrar, and adds tools as the server lists them.
func TestDynamicSourcePublishesIntoTheRegistry(t *testing.T) {
	source := &dynamicSource{}
	registry, err := tool.NewRegistry([]tool.Source{source})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	defer func() { _ = registry.Close() }()

	if _, ok := registry.Get("remote"); ok {
		t.Fatal("a tool appeared before the source published it")
	}
	if err := source.publish("remote"); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if _, ok := registry.Get("remote"); !ok {
		t.Fatal("a published tool is not in the registry")
	}
	if got := len(registry.Definitions()); got != 1 {
		t.Fatalf("definitions = %d, want 1", got)
	}

	// The definition epoch is what makes a cached projection notice the change.
	before := registry.DefinitionsEpoch()
	source.remove("remote")
	if registry.DefinitionsEpoch() == before {
		t.Fatal("removing a tool did not move the definition epoch")
	}
	if _, ok := registry.Get("remote"); ok {
		t.Fatal("a removed tool is still in the registry")
	}
}

// TestDetachOnClose keeps a long-lived source from holding every registry it ever served:
// closing the registry must hand it back.
func TestDetachOnClose(t *testing.T) {
	source := &dynamicSource{}
	registry, err := tool.NewRegistry([]tool.Source{source})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	source.mu.Lock()
	detached := source.detached
	stillAttached := source.attached != nil
	source.mu.Unlock()
	if detached != 1 {
		t.Fatalf("Detach called %d times, want 1", detached)
	}
	if stillAttached {
		t.Fatal("the source kept the closed registry's registrar")
	}
}

// TestPublishAfterCloseIsRefused: a background connect that finishes after the registry
// closed must not resurrect a tool nobody can dispatch.
func TestPublishAfterCloseIsRefused(t *testing.T) {
	source := &dynamicSource{}
	registry, err := tool.NewRegistry([]tool.Source{source})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	// Keep the registrar past Detach to model a goroutine that captured it.
	source.mu.Lock()
	registrar := source.attached
	source.mu.Unlock()
	if err := registry.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := registrar.Add(simpleTool("late")); err == nil {
		t.Fatal("a closed registry accepted a tool")
	}
}
