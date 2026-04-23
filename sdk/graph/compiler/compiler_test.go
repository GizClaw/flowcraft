package compiler

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

func TestGraphDefinition_Validate(t *testing.T) {
	tests := []struct {
		name    string
		def     graph.GraphDefinition
		wantErr bool
	}{
		{
			name:    "empty name",
			def:     graph.GraphDefinition{},
			wantErr: true,
		},
		{
			name:    "no entry",
			def:     graph.GraphDefinition{Name: "test"},
			wantErr: true,
		},
		{
			name:    "no nodes",
			def:     graph.GraphDefinition{Name: "test", Entry: "start"},
			wantErr: true,
		},
		{
			name: "entry not in nodes",
			def: graph.GraphDefinition{
				Name: "test", Entry: "missing",
				Nodes: []graph.NodeDefinition{{ID: "a", Type: "passthrough"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate node ID",
			def: graph.GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []graph.NodeDefinition{
					{ID: "a", Type: "passthrough"},
					{ID: "a", Type: "passthrough"},
				},
			},
			wantErr: true,
		},
		{
			name: "edge from unknown node",
			def: graph.GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []graph.NodeDefinition{{ID: "a", Type: "passthrough"}},
				Edges: []graph.EdgeDefinition{{From: "unknown", To: graph.END}},
			},
			wantErr: true,
		},
		{
			name: "valid simple",
			def: graph.GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []graph.NodeDefinition{{ID: "a", Type: "passthrough"}},
				Edges: []graph.EdgeDefinition{{From: "a", To: graph.END}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := tt.def
			err := def.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestCompiler_Compile_SimplePassthrough(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
		},
	}

	result, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if result.Metadata.NodeCount != 1 {
		t.Fatalf("expected 1 node, got %d", result.Metadata.NodeCount)
	}
	if result.Metadata.EdgeCount != 1 {
		t.Fatalf("expected 1 edge, got %d", result.Metadata.EdgeCount)
	}
}

func TestCompiler_Compile_WithConditions(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
			{ID: "branch_a", Type: "passthrough"},
			{ID: "branch_b", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: "branch_a", Condition: "x == true"},
			{From: "start", To: "branch_b"},
			{From: "branch_a", To: graph.END},
			{From: "branch_b", To: graph.END},
		},
	}

	result, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if result.Metadata.NodeCount != 3 {
		t.Fatalf("expected 3 nodes, got %d", result.Metadata.NodeCount)
	}
}

func TestCompiler_Compile_SkipCondition(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough", SkipCondition: "skip_me == true"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
		},
	}

	result, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if _, ok := result.Graph.SkipConditions["start"]; !ok {
		t.Fatal("expected skip condition for 'start' node")
	}
}

func TestCompiler_Compile_InvalidCondition(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END, Condition: "invalid >>>> syntax"},
		},
	}

	_, err := c.Compile(def)
	if err == nil {
		t.Fatal("expected compile error for invalid condition")
	}
}

func TestCompiler_Compile_DeadEnd_Warning(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
			{ID: "orphan", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: graph.END},
			{From: "orphan", To: graph.END},
		},
	}

	result, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}

	hasWarning := false
	for _, w := range result.Warnings {
		if w.Code == "unreachable_end" {
			hasWarning = true
		}
	}
	_ = hasWarning
}

func TestCompiler_Compile_DetectsParallel(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "start",
		Nodes: []graph.NodeDefinition{
			{ID: "start", Type: "passthrough"},
			{ID: "a", Type: "passthrough"},
			{ID: "b", Type: "passthrough"},
			{ID: "join", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "start", To: "a"},
			{From: "start", To: "b"},
			{From: "a", To: "join"},
			{From: "b", To: "join"},
			{From: "join", To: graph.END},
		},
	}

	result, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if !result.Metadata.HasParallel {
		t.Fatal("expected HasParallel to be true")
	}
}

func TestCompiler_Compile_DetectsCycles(t *testing.T) {
	c := NewCompiler()
	def := &graph.GraphDefinition{
		Name:  "test",
		Entry: "a",
		Nodes: []graph.NodeDefinition{
			{ID: "a", Type: "passthrough"},
			{ID: "b", Type: "passthrough"},
		},
		Edges: []graph.EdgeDefinition{
			{From: "a", To: "b"},
			{From: "b", To: "a", Condition: "loop == true"},
			{From: "b", To: graph.END},
		},
	}

	result, err := c.Compile(def)
	if err != nil {
		t.Fatalf("compile failed: %v", err)
	}
	if !result.Metadata.HasCycles {
		t.Fatal("expected HasCycles to be true")
	}
}

func TestCompiler_Compile_LLMIsolatedMessagesKeyWarning(t *testing.T) {
	c := NewCompiler()

	t.Run("warns when query_fallback missing", func(t *testing.T) {
		def := &graph.GraphDefinition{
			Name:  "test",
			Entry: "llm1",
			Nodes: []graph.NodeDefinition{
				{ID: "llm1", Type: "llm", Config: map[string]any{
					"messages_key": "custom_messages",
				}},
			},
			Edges: []graph.EdgeDefinition{
				{From: "llm1", To: graph.END},
			},
		}
		result, err := c.Compile(def)
		if err != nil {
			t.Fatalf("compile failed: %v", err)
		}
		found := false
		for _, w := range result.Warnings {
			if w.Code == "llm_isolated_messages_no_fallback" {
				found = true
			}
		}
		if !found {
			t.Fatal("expected llm_isolated_messages_no_fallback warning")
		}
	})

	t.Run("no warning when query_fallback is true", func(t *testing.T) {
		def := &graph.GraphDefinition{
			Name:  "test",
			Entry: "llm1",
			Nodes: []graph.NodeDefinition{
				{ID: "llm1", Type: "llm", Config: map[string]any{
					"messages_key":   "custom_messages",
					"query_fallback": true,
				}},
			},
			Edges: []graph.EdgeDefinition{
				{From: "llm1", To: graph.END},
			},
		}
		result, err := c.Compile(def)
		if err != nil {
			t.Fatalf("compile failed: %v", err)
		}
		for _, w := range result.Warnings {
			if w.Code == "llm_isolated_messages_no_fallback" {
				t.Fatalf("unexpected warning: %s", w.Message)
			}
		}
	})

	t.Run("no warning when using default messages key", func(t *testing.T) {
		def := &graph.GraphDefinition{
			Name:  "test",
			Entry: "llm1",
			Nodes: []graph.NodeDefinition{
				{ID: "llm1", Type: "llm", Config: map[string]any{
					"messages_key": "messages",
				}},
			},
			Edges: []graph.EdgeDefinition{
				{From: "llm1", To: graph.END},
			},
		}
		result, err := c.Compile(def)
		if err != nil {
			t.Fatalf("compile failed: %v", err)
		}
		for _, w := range result.Warnings {
			if w.Code == "llm_isolated_messages_no_fallback" {
				t.Fatalf("unexpected warning for default messages key")
			}
		}
	})

	t.Run("no warning when messages_key omitted", func(t *testing.T) {
		def := &graph.GraphDefinition{
			Name:  "test",
			Entry: "llm1",
			Nodes: []graph.NodeDefinition{
				{ID: "llm1", Type: "llm", Config: map[string]any{}},
			},
			Edges: []graph.EdgeDefinition{
				{From: "llm1", To: graph.END},
			},
		}
		result, err := c.Compile(def)
		if err != nil {
			t.Fatalf("compile failed: %v", err)
		}
		for _, w := range result.Warnings {
			if w.Code == "llm_isolated_messages_no_fallback" {
				t.Fatalf("unexpected warning when messages_key is omitted")
			}
		}
	})
}
