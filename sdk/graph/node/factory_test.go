package node

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func dummyBuilder(id, typ string) NodeBuilder {
	return func(def graph.NodeDefinition) (graph.Node, error) {
		return graph.NewPassthroughNode(id, typ), nil
	}
}

func TestFactory_RegisterAndBuild(t *testing.T) {
	f := NewFactory()
	f.RegisterBuilder("custom", dummyBuilder("c1", "custom"))

	n, err := f.Build(graph.NodeDefinition{ID: "c1", Type: "custom"})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if n.ID() != "c1" {
		t.Fatalf("ID = %q, want c1", n.ID())
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

	n, err := f.Build(graph.NodeDefinition{ID: "fb", Type: "any"})
	if err != nil {
		t.Fatalf("Build with fallback: %v", err)
	}
	if n.ID() != "fb" {
		t.Fatalf("ID = %q, want fb", n.ID())
	}
}

func TestFactory_Fallback_Getter(t *testing.T) {
	f := NewFactory()
	if f.Fallback() != nil {
		t.Fatal("Fallback should be nil initially")
	}
	f.SetFallback(dummyBuilder("fb", "t"))
	if f.Fallback() == nil {
		t.Fatal("Fallback should not be nil after SetFallback")
	}
}

func TestFactory_BuildEndNode(t *testing.T) {
	f := NewFactory()
	n, err := f.Build(graph.NodeDefinition{ID: "end", Type: "__end__"})
	if err != nil {
		t.Fatalf("Build __end__: %v", err)
	}
	if n.ID() != graph.END {
		t.Fatalf("end node ID = %q, want %q", n.ID(), graph.END)
	}
}

func TestFactory_BuildPassthrough(t *testing.T) {
	f := NewFactory()
	n, err := f.Build(graph.NodeDefinition{ID: "pt1", Type: "passthrough"})
	if err != nil {
		t.Fatalf("Build passthrough: %v", err)
	}
	if n.ID() != "pt1" {
		t.Fatalf("passthrough ID = %q", n.ID())
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

func TestPortsForType_Unknown(t *testing.T) {
	input, output := PortsForType("__no_such_type__")
	if input != nil || output != nil {
		t.Fatalf("expected nil ports for unknown type, got input=%v output=%v", input, output)
	}
}

func TestRegisterPorts_RoundTrip(t *testing.T) {
	in := []graph.Port{{Name: "in", Type: graph.PortTypeString}}
	out := []graph.Port{{Name: "out", Type: graph.PortTypeBool}}
	RegisterPorts("__test_register_ports__", in, out)
	gotIn, gotOut := PortsForType("__test_register_ports__")
	if len(gotIn) != 1 || gotIn[0].Name != "in" {
		t.Fatalf("input ports = %v", gotIn)
	}
	if len(gotOut) != 1 || gotOut[0].Name != "out" {
		t.Fatalf("output ports = %v", gotOut)
	}
}
