package graph

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestGraphDefinition_Validate(t *testing.T) {
	tests := []struct {
		name    string
		def     GraphDefinition
		wantErr bool
	}{
		{
			name:    "empty name",
			def:     GraphDefinition{},
			wantErr: true,
		},
		{
			name:    "no entry",
			def:     GraphDefinition{Name: "test"},
			wantErr: true,
		},
		{
			name:    "no nodes",
			def:     GraphDefinition{Name: "test", Entry: "start"},
			wantErr: true,
		},
		{
			name: "empty node ID",
			def: GraphDefinition{
				Name: "test", Entry: "start",
				Nodes: []NodeDefinition{{ID: "", Type: "passthrough"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate node ID",
			def: GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []NodeDefinition{
					{ID: "a", Type: "passthrough"},
					{ID: "a", Type: "passthrough"},
				},
			},
			wantErr: true,
		},
		{
			name: "entry not in nodes",
			def: GraphDefinition{
				Name: "test", Entry: "missing",
				Nodes: []NodeDefinition{{ID: "a", Type: "passthrough"}},
			},
			wantErr: true,
		},
		{
			name: "edge from unknown node",
			def: GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []NodeDefinition{{ID: "a", Type: "passthrough"}},
				Edges: []EdgeDefinition{{From: "unknown", To: END}},
			},
			wantErr: true,
		},
		{
			name: "edge to unknown node",
			def: GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []NodeDefinition{{ID: "a", Type: "passthrough"}},
				Edges: []EdgeDefinition{{From: "a", To: "unknown"}},
			},
			wantErr: true,
		},
		{
			name: "edge to END is valid",
			def: GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []NodeDefinition{{ID: "a", Type: "passthrough"}},
				Edges: []EdgeDefinition{{From: "a", To: END}},
			},
			wantErr: false,
		},
		{
			name: "valid multi-node graph",
			def: GraphDefinition{
				Name: "test", Entry: "a",
				Nodes: []NodeDefinition{
					{ID: "a", Type: "passthrough"},
					{ID: "b", Type: "passthrough"},
				},
				Edges: []EdgeDefinition{
					{From: "a", To: "b"},
					{From: "b", To: END},
				},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.def.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err != nil && !errdefs.IsValidation(err) {
				t.Errorf("Validate() should return validation error, got %T: %v", err, err)
			}
		})
	}
}

func TestGraphDefinition_Validate_ReturnsStructuredErrors(t *testing.T) {
	def := GraphDefinition{}
	err := def.Validate()
	if err == nil {
		t.Fatal("expected error")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected errdefs.Validation error, got %T: %v", err, err)
	}
}
