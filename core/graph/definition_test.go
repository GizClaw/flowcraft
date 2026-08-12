package graph

import (
	"testing"
)

func TestGraphDefinitionValidate(t *testing.T) {
	valid := &GraphDefinition{
		Name:  "g",
		Entry: "a",
		Nodes: []NodeDefinition{{ID: "a", Type: "echo"}, {ID: "b", Type: "echo"}},
		Edges: []EdgeDefinition{{From: "a", To: "b"}, {From: "b", To: END}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}

	cases := []struct {
		name string
		mut  func(d *GraphDefinition)
	}{
		{"missing name", func(d *GraphDefinition) { d.Name = "" }},
		{"missing entry", func(d *GraphDefinition) { d.Entry = "" }},
		{"unknown entry", func(d *GraphDefinition) { d.Entry = "zzz" }},
		{"no nodes", func(d *GraphDefinition) { d.Nodes = nil }},
		{"duplicate node id", func(d *GraphDefinition) { d.Nodes[1].ID = "a" }},
		{"node without id", func(d *GraphDefinition) { d.Nodes[0].ID = "" }},
		{"node without type", func(d *GraphDefinition) { d.Nodes[0].Type = "" }},
		{"edge from unknown", func(d *GraphDefinition) { d.Edges[0].From = "zzz" }},
		{"edge to unknown", func(d *GraphDefinition) { d.Edges[0].To = "zzz" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := *valid
			d.Nodes = append([]NodeDefinition(nil), valid.Nodes...)
			d.Edges = append([]EdgeDefinition(nil), valid.Edges...)
			tc.mut(&d)
			if err := d.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}
}
