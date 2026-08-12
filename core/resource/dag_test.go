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

func TestGraphDuplicateNode(t *testing.T) {
	g := NewGraph()
	_ = g.Add("a", resourceWithDeps(nil))
	if err := g.Add("a", resourceWithDeps(nil)); !errdefs.IsConflict(err) {
		t.Fatalf("duplicate Add error = %v, want conflict", err)
	}
}
