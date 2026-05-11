package graph

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

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

// TestBoard_ValidateOutputs_VarBacked covers the historical contract:
// a required output port resolves through a board variable of the same
// name. This is what every PortTypeString / PortTypeBool / PortTypeUsage
// output relies on.
func TestBoard_ValidateOutputs_VarBacked(t *testing.T) {
	b := NewBoard()
	b.SetVar("response", "hi")

	node := &testPortNode{
		outputs: []Port{
			{Name: "response", Type: PortTypeString, Required: true},
			{Name: "optional", Type: PortTypeBool, Required: false},
		},
	}

	if err := ValidateOutputs(b, node); err != nil {
		t.Fatalf("should pass: %v", err)
	}

	node.outputs = append(node.outputs, Port{Name: "missing", Type: PortTypeString, Required: true})
	if err := ValidateOutputs(b, node); err == nil {
		t.Fatal("should fail for missing required output")
	}
}

// TestBoard_ValidateOutputs_MessagesChannelFallback regression-guards
// issue #87: under the v0.3 messages-only-on-channel contract, llmnode
// writes the assistant reply via board.SetChannel and never SetVar. A
// required PortTypeMessages output must therefore be satisfiable by a
// non-empty channel of the same name, exactly like ValidateInputs.
// Without the fallback, every llmnode round through graph/runner is
// classified Status=failed despite producing the correct messages.
func TestBoard_ValidateOutputs_MessagesChannelFallback(t *testing.T) {
	b := NewBoard()
	b.SetChannel(MainChannel, []model.Message{
		model.NewTextMessage(model.RoleAssistant, "hi"),
	})

	node := &testPortNode{
		outputs: []Port{
			{Name: MainChannel, Type: PortTypeMessages, Required: true},
		},
	}

	if err := ValidateOutputs(b, node); err != nil {
		t.Fatalf("PortTypeMessages output should be satisfied by a non-empty channel: %v", err)
	}

	// Empty channel must still be treated as missing — the fallback is
	// "non-empty", not "channel exists". Otherwise a node that
	// declares a required messages output but writes nothing would
	// silently pass validation.
	b2 := NewBoard()
	if err := ValidateOutputs(b2, node); err == nil {
		t.Fatal("empty channel must not satisfy a required PortTypeMessages output")
	}
}

type testPortNode struct {
	inputs  []Port
	outputs []Port
}

func (n *testPortNode) InputPorts() []Port  { return n.inputs }
func (n *testPortNode) OutputPorts() []Port { return n.outputs }
