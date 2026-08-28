package graph

import (
	"testing"
	"time"
)

func TestBuildRejectsNonPositiveRunEndPublishTimeout(t *testing.T) {
	reg := newTestRegistry(t)
	def := &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}},
	}
	for _, timeout := range []time.Duration{0, -time.Millisecond} {
		if _, err := Build(def, reg, WithRunEndPublishTimeout(timeout)); err == nil {
			t.Fatalf("run-end publish timeout %s accepted", timeout)
		}
	}
}

func TestBuildRejectsUnknownConfigField(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := Build(&GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{
			ID: "a", Type: "echo",
			Config: []byte(`{"not_a_field": 1}`),
		}},
	}, reg)
	if err == nil {
		t.Fatal("unknown config field accepted")
	}
}

func TestBuildRejectsBadConditionSyntax(t *testing.T) {
	reg := newTestRegistry(t)
	def := &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: "b", Condition: "len("}},
	}
	if _, err := Build(def, reg); err == nil {
		t.Fatal("invalid edge condition accepted")
	}

	def.Edges[0].Condition = ""
	def.Nodes[0].SkipCondition = "len("
	if _, err := Build(def, reg); err == nil {
		t.Fatal("invalid skip condition accepted")
	}
}

func TestBuildRejectsReferenceRoleKey(t *testing.T) {
	reg := newTestRegistry(t)
	_, err := Build(&GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{
			ID: "a", Type: "echo",
			Config: []byte(`{"set_var": "${board:dynamic}"}`),
		}},
	}, reg)
	if err == nil {
		t.Fatal("reference-valued role key accepted")
	}
}

func TestBuildStatsAndAccessors(t *testing.T) {
	reg := newTestRegistry(t)
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}, reg)

	if g.Name() != "g" || g.Entry() != "a" {
		t.Fatal("accessors wrong")
	}
	if ids := g.NodeIDs(); len(ids) != 2 || ids[0] != "a" {
		t.Fatalf("NodeIDs = %v", ids)
	}
	if edges := g.EdgesFrom("a"); len(edges) != 1 || edges[0].To != "b" {
		t.Fatalf("EdgesFrom = %+v", edges)
	}
	stats := g.Stats()
	if stats.Nodes != 2 || stats.Edges != 2 || stats.NodeTypes != 1 || stats.ParallelEnabled {
		t.Fatalf("Stats = %+v", stats)
	}
}

func TestAnalyzeWarnings(t *testing.T) {
	reg := newTestRegistry(t)

	hasKind := func(warnings []Warning, kind WarningKind) bool {
		for _, w := range warnings {
			if w.Kind == kind {
				return true
			}
		}
		return false
	}

	// Unreachable + dead-end.
	g := mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"},
			{ID: "orphan", Type: "echo"},
		},
		Edges: []EdgeDefinition{{From: "a", To: END}},
	}, reg)
	if !hasKind(g.Warnings(), WarningUnreachableNode) {
		t.Fatal("unreachable node not flagged")
	}
	if !hasKind(g.Warnings(), WarningDeadEndNode) {
		t.Fatal("dead-end node not flagged")
	}

	// Unguarded cycle: a → b → a, all unconditional.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: "a"}},
	}, reg)
	if !hasKind(g.Warnings(), WarningUnguardedCycle) {
		t.Fatal("unguarded cycle not flagged")
	}

	// Guarded cycle: b → a is conditional.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"},
			{From: "b", To: "a", Condition: "again == true"},
			{From: "b", To: END},
		},
	}, reg)
	if hasKind(g.Warnings(), WarningUnguardedCycle) {
		t.Fatal("guarded cycle flagged as unguarded")
	}

	// Missing default branch.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}, {ID: "c", Type: "echo"}},
		Edges: []EdgeDefinition{
			{From: "a", To: "b", Condition: "x == 1"},
			{From: "a", To: "c", Condition: "x == 2"},
			{From: "b", To: END},
			{From: "c", To: END},
		},
	}, reg)
	if !hasKind(g.Warnings(), WarningMissingDefaultBranch) {
		t.Fatal("missing default branch not flagged")
	}

	// Unresolved reference: config reads a var nobody writes.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{
			ID: "a", Type: "echo",
			Config: []byte(`{"set_var": "out", "message": "${board:missing_input}"}`),
		}},
	}, reg)
	if !hasKind(g.Warnings(), WarningUnresolvedReference) {
		t.Fatal("unresolved reference not flagged")
	}

	// Resolved reference: upstream node writes the var.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo", Config: []byte(`{"set_var": "input", "set_val": 1}`)},
			{ID: "b", Type: "echo", Config: []byte(`{"message": "${board:input}"}`)},
		},
		Edges: []EdgeDefinition{{From: "a", To: "b"}},
	}, reg)
	if hasKind(g.Warnings(), WarningUnresolvedReference) {
		t.Fatal("resolved reference flagged")
	}

	// Parallel no join: a forks to b and c which never meet.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}, {ID: "c", Type: "echo"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: END}, {From: "c", To: END},
		},
	}, reg)
	if !hasKind(g.Warnings(), WarningParallelNoJoin) {
		t.Fatal("parallel-no-join not flagged")
	}

	// Joined fan-out: b and c both reach d.
	g = mustBuild(t, &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{
			{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"},
			{ID: "c", Type: "echo"}, {ID: "d", Type: "echo"},
		},
		Edges: []EdgeDefinition{
			{From: "a", To: "b"}, {From: "a", To: "c"},
			{From: "b", To: "d"}, {From: "c", To: "d"},
			{From: "d", To: END},
		},
	}, reg)
	if hasKind(g.Warnings(), WarningParallelNoJoin) {
		t.Fatal("joined fan-out flagged")
	}
}
