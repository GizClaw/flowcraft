package node

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func dummyBuilder(id, typ string) NodeBuilder {
	return func(def graph.NodeDefinition, bctx *BuildContext) (graph.Node, error) {
		return graph.NewPassthroughNode(id, typ), nil
	}
}

func TestFactory_RegisterAndBuild(t *testing.T) {
	f := NewFactory()
	f.RegisterBuilder("custom", dummyBuilder("c1", "custom"))

	node, err := f.Build(graph.NodeDefinition{ID: "c1", Type: "custom"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if node.ID() != "c1" {
		t.Fatalf("ID = %q, want c1", node.ID())
	}
}

func TestFactory_BuildUnknownType(t *testing.T) {
	f := NewFactory()
	_, err := f.Build(graph.NodeDefinition{ID: "x", Type: "unknown"})
	if err == nil {
		t.Fatal("expected error for unknown type")
	}
}

func TestFactory_FallbackBuilder(t *testing.T) {
	f := NewFactory()
	f.SetFallback(dummyBuilder("fb", "fallback"))

	node, err := f.Build(graph.NodeDefinition{ID: "fb", Type: "any"})
	if err != nil {
		t.Fatalf("Build with fallback: %v", err)
	}
	if node.ID() != "fb" {
		t.Fatalf("ID = %q, want fb", node.ID())
	}
}

func TestFactory_Fallback_Getter(t *testing.T) {
	f := NewFactory()
	if f.Fallback() != nil {
		t.Fatal("Fallback should be nil initially (after clearing default)")
	}

	fb := dummyBuilder("fb", "t")
	f.SetFallback(fb)
	if f.Fallback() == nil {
		t.Fatal("Fallback should not be nil after SetFallback")
	}
}

func TestFactory_ConcurrentRegisterAndBuild(t *testing.T) {
	f := NewFactory()
	const n = 50
	var wg sync.WaitGroup
	errs := make(chan error, n*2)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			typ := fmt.Sprintf("type_%d", idx)
			f.RegisterBuilder(typ, dummyBuilder(typ, typ))
		}(i)
	}

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			typ := fmt.Sprintf("type_%d", idx)
			_, err := f.Build(graph.NodeDefinition{ID: typ, Type: typ})
			if err != nil && !strings.Contains(err.Error(), "unknown node type") {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent error: %v", err)
	}
}

func TestFactory_BuildEndNode(t *testing.T) {
	f := NewFactory()
	node, err := f.Build(graph.NodeDefinition{ID: "end", Type: "__end__"})
	if err != nil {
		t.Fatalf("Build __end__: %v", err)
	}
	if node.ID() != graph.END {
		t.Fatalf("end node ID = %q, want %q", node.ID(), graph.END)
	}
}

func TestFactory_BuildPassthrough(t *testing.T) {
	f := NewFactory()
	node, err := f.Build(graph.NodeDefinition{ID: "pt1", Type: "passthrough"})
	if err != nil {
		t.Fatalf("Build passthrough: %v", err)
	}
	if node.ID() != "pt1" {
		t.Fatalf("passthrough ID = %q, want pt1", node.ID())
	}
}

func emptyFactory() *Factory {
	return &Factory{
		builders: make(map[string]NodeBuilder),
		buildCtx: &BuildContext{},
	}
}

func TestFactory_ValidateConsistency_AllMatch(t *testing.T) {
	f := emptyFactory()
	f.RegisterBuilder("a", dummyBuilder("a", "a"))
	f.RegisterBuilder("b", dummyBuilder("b", "b"))

	schemas := NewSchemaRegistry()
	schemas.Register(NodeSchema{Type: "a", Label: "A"})
	schemas.Register(NodeSchema{Type: "b", Label: "B"})

	warnings := f.ValidateConsistency(schemas)
	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", warnings)
	}
}

func TestFactory_ValidateConsistency_MissingSchema(t *testing.T) {
	f := emptyFactory()
	f.RegisterBuilder("has_builder", dummyBuilder("x", "x"))

	schemas := NewSchemaRegistry()

	warnings := f.ValidateConsistency(schemas)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "has_builder") || !strings.Contains(warnings[0], "no schema") {
		t.Fatalf("unexpected warning: %q", warnings[0])
	}
}

func TestFactory_ValidateConsistency_MissingBuilder(t *testing.T) {
	f := emptyFactory()

	schemas := NewSchemaRegistry()
	schemas.Register(NodeSchema{Type: "has_schema", Label: "X"})

	warnings := f.ValidateConsistency(schemas)
	if len(warnings) != 1 {
		t.Fatalf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if !strings.Contains(warnings[0], "has_schema") || !strings.Contains(warnings[0], "no builder") {
		t.Fatalf("unexpected warning: %q", warnings[0])
	}
}

func TestFactory_ValidateConsistency_EndNodeSkipped(t *testing.T) {
	f := emptyFactory()

	schemas := NewSchemaRegistry()
	schemas.Register(NodeSchema{Type: "__end__", Label: "End"})

	warnings := f.ValidateConsistency(schemas)
	for _, w := range warnings {
		if strings.Contains(w, "__end__") {
			t.Fatalf("__end__ should be skipped, got warning: %q", w)
		}
	}
}

func TestFactory_ValidateConsistency_BothMissing(t *testing.T) {
	f := emptyFactory()
	f.RegisterBuilder("only_builder", dummyBuilder("x", "x"))

	schemas := NewSchemaRegistry()
	schemas.Register(NodeSchema{Type: "only_schema", Label: "X"})

	warnings := f.ValidateConsistency(schemas)
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %d: %v", len(warnings), warnings)
	}
	hasBuilder, hasSchema := false, false
	for _, w := range warnings {
		if strings.Contains(w, "only_builder") {
			hasBuilder = true
		}
		if strings.Contains(w, "only_schema") {
			hasSchema = true
		}
	}
	if !hasBuilder || !hasSchema {
		t.Fatalf("expected warnings for both sides, got %v", warnings)
	}
}
