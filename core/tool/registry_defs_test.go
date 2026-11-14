package tool

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// schemaTool builds its schema per call, like every builtin tool does.
type schemaTool struct{ name string }

func (t schemaTool) Definition() message.ToolDefinition {
	raw, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path": map[string]any{"type": "string", "description": "a path"},
		},
		"required": []string{"path"},
	})
	if err != nil {
		panic(err)
	}
	return message.ToolDefinition{Name: t.name, Description: "tool " + t.name, InputSchema: raw}
}

func (schemaTool) Execute(context.Context, string) (message.Content, error) {
	return message.NewTextContent("ok"), nil
}

func (schemaTool) Metadata() ToolMeta { return ToolMeta{} }

func registryWith(t *testing.T, n int) *Registry {
	t.Helper()
	registry, err := NewRegistry(nil)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	for i := 0; i < n; i++ {
		if err := registry.Add(schemaTool{name: "tool" + string(rune('a'+i))}); err != nil {
			t.Fatalf("Add: %v", err)
		}
	}
	return registry
}

// The loop asks for the whole set every round; rebuilding twelve schemas each
// time is the cost this cache exists to remove.
func TestDefinitionsAreCached(t *testing.T) {
	registry := registryWith(t, 12)
	// Warm the cache.
	_ = registry.Definitions()

	calls := func() { _ = registry.Definitions() }
	if got := testing.AllocsPerRun(100, calls); got > 2 {
		t.Fatalf("Definitions allocates %.0f times per call, want at most 2 "+
			"(one copy plus slack); a rebuild would be dozens", got)
	}
}

// Changing the set must drop the cache, or the model is shown tools that no
// longer exist.
func TestAddAndRemoveInvalidateDefinitions(t *testing.T) {
	registry := registryWith(t, 1)
	if got := len(registry.Definitions()); got != 1 {
		t.Fatalf("definitions = %d, want 1", got)
	}
	if err := registry.Add(schemaTool{name: "added"}); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if got := len(registry.Definitions()); got != 2 {
		t.Errorf("definitions = %d after Add, want 2", got)
	}
	registry.Remove("added")
	if got := len(registry.Definitions()); got != 1 {
		t.Errorf("definitions = %d after Remove, want 1", got)
	}
}

func BenchmarkDefinitions(b *testing.B) {
	registry, err := NewRegistry(nil)
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < 12; i++ {
		if err := registry.Add(schemaTool{name: "tool" + string(rune('a'+i))}); err != nil {
			b.Fatal(err)
		}
	}
	_ = registry.Definitions()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = registry.Definitions()
	}
}
