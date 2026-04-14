package graph

import "testing"

func TestBoard_ValidateInputs(t *testing.T) {
	b := NewBoard()
	b.SetVar("query", "hello")

	node := &testPortNode{
		inputs: []Port{
			{Name: "query", Type: PortTypeString, Required: true},
			{Name: "optional", Type: PortTypeString, Required: false},
		},
	}

	if err := ValidateInputs(b, node); err != nil {
		t.Fatalf("should pass: %v", err)
	}

	node.inputs = append(node.inputs, Port{Name: "missing", Type: PortTypeString, Required: true})
	if err := ValidateInputs(b, node); err == nil {
		t.Fatal("should fail for missing required input")
	}
}

type testPortNode struct {
	inputs  []Port
	outputs []Port
}

func (n *testPortNode) InputPorts() []Port  { return n.inputs }
func (n *testPortNode) OutputPorts() []Port { return n.outputs }
