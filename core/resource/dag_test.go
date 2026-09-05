package resource

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

func resourceWithDeps(deps Deps) Resource {
	return Resource{Kind: "test.Kind", Deps: deps}
}

func TestGraphValidateDanglingRef(t *testing.T) {
	g := NewGraph()
	_ = g.Add("box", resourceWithDeps(Deps{"workspace": "missing"}))
	if err := g.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("Validate error = %v, want validation for dangling ref", err)
	}
}

func TestGraphValidateCycle(t *testing.T) {
	g := NewGraph()
	_ = g.Add("a", resourceWithDeps(Deps{"dep": "b"}))
	_ = g.Add("b", resourceWithDeps(Deps{"dep": "a"}))
	if err := g.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("Validate error = %v, want validation for cycle", err)
	}
}

func TestGraphTopoOrder(t *testing.T) {
	g := NewGraph()
	_ = g.Add("kit", resourceWithDeps(Deps{"sandbox": "box", "inference": "infer"}))
	_ = g.Add("infer", resourceWithDeps(nil))
	_ = g.Add("box", resourceWithDeps(Deps{"workspace": "fs"}))
	_ = g.Add("fs", resourceWithDeps(nil))

	order, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	pos := make(map[string]int, len(order))
	for i, name := range order {
		pos[name] = i
	}
	if pos["fs"] > pos["box"] || pos["box"] > pos["kit"] || pos["infer"] > pos["kit"] {
		t.Fatalf("order violates dependencies: %v", order)
	}
}

func TestGraphTopoOrderDeterministic(t *testing.T) {
	g := NewGraph()
	for _, name := range []string{"a", "z", "b", "c", "m", "n"} {
		var deps Deps
		switch name {
		case "b", "c":
			deps = Deps{"dep": "a"}
		case "m", "n":
			deps = Deps{"dep": "z"}
		}
		if err := g.Add(name, resourceWithDeps(deps)); err != nil {
			t.Fatalf("Add(%q): %v", name, err)
		}
	}

	order, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	want := []string{"a", "b", "c", "z", "m", "n"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("TopoOrder = %v, want %v", order, want)
		}
	}
}

func TestGraphDuplicateNode(t *testing.T) {
	g := NewGraph()
	_ = g.Add("a", resourceWithDeps(nil))
	if err := g.Add("a", resourceWithDeps(nil)); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate Add error = %v, want conflict", err)
	}
}

func TestGraphExternalLeafResolvesDepsWithoutEnteringOrder(t *testing.T) {
	g := NewGraph()
	if err := g.AddExternal("db"); err != nil {
		t.Fatalf("AddExternal: %v", err)
	}
	if err := g.Add("consumer", resourceWithDeps(Deps{"db": "db"})); err != nil {
		t.Fatalf("Add: %v", err)
	}
	order, err := g.TopoOrder()
	if err != nil {
		t.Fatalf("TopoOrder: %v", err)
	}
	if len(order) != 1 || order[0] != "consumer" {
		t.Fatalf("TopoOrder = %v, want [consumer] only", order)
	}
}

func TestGraphExternalDuplicateAndResourceCollision(t *testing.T) {
	g := NewGraph()
	if err := g.AddExternal("db"); err != nil {
		t.Fatalf("AddExternal: %v", err)
	}
	if err := g.AddExternal("db"); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate AddExternal error = %v, want conflict", err)
	}
	if err := g.Add("db", resourceWithDeps(nil)); !errdefs.IsConflict(err) {
		t.Fatalf("Add over external error = %v, want conflict", err)
	}

	other := NewGraph()
	if err := other.Add("db", resourceWithDeps(nil)); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if err := other.AddExternal("db"); !errdefs.IsConflict(err) {
		t.Fatalf("AddExternal over resource error = %v, want conflict", err)
	}
}

func TestGraphExternalInvalidName(t *testing.T) {
	g := NewGraph()
	for _, name := range []string{"", "db/item", "  "} {
		if err := g.AddExternal(name); !errdefs.IsValidation(err) {
			t.Fatalf("AddExternal(%q) error = %v, want validation", name, err)
		}
	}
}
