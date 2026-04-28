package scriptnode

import (
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node"
)

// init declares the input/output port shape of every built-in jsnode type.
// This is pure metadata — no I/O, no global builder side-effects — and is
// consumed by ScriptNode.New via node.PortsForType when assembling a node.
//
// New jsnode types should add their entry here; explicit registration is
// preferred over reflection-based schema lookup.
func init() {
	node.RegisterPorts("router",
		[]graph.Port{{Name: "routes", Type: graph.PortTypeAny, Required: true}},
		[]graph.Port{{Name: "route_target", Type: graph.PortTypeString, Required: true}},
	)
	node.RegisterPorts("ifelse",
		[]graph.Port{{Name: "conditions", Type: graph.PortTypeAny, Required: true}},
		[]graph.Port{{Name: "branch_result", Type: graph.PortTypeString, Required: true}},
	)
	node.RegisterPorts("template",
		[]graph.Port{{Name: "template", Type: graph.PortTypeString, Required: true}},
		[]graph.Port{{Name: "template_output", Type: graph.PortTypeString, Required: true}},
	)
	node.RegisterPorts("answer",
		[]graph.Port{
			{Name: "template", Type: graph.PortTypeString},
			{Name: "keys", Type: graph.PortTypeAny},
		},
		[]graph.Port{{Name: "answer", Type: graph.PortTypeString, Required: true}},
	)
	node.RegisterPorts("assigner",
		[]graph.Port{{Name: "assignments", Type: graph.PortTypeAny, Required: true}},
		nil,
	)
	node.RegisterPorts("loopguard",
		nil,
		[]graph.Port{
			{Name: "loop_count", Type: graph.PortTypeInteger, Required: true},
			{Name: "loop_count_exceeded", Type: graph.PortTypeBool, Required: true},
		},
	)
	node.RegisterPorts("aggregator",
		[]graph.Port{{Name: "input_keys", Type: graph.PortTypeAny, Required: true}},
		[]graph.Port{{Name: "aggregated", Type: graph.PortTypeAny}},
	)
	node.RegisterPorts("gate",
		[]graph.Port{{Name: "commands", Type: graph.PortTypeAny, Required: true}},
		[]graph.Port{
			{Name: "gate_result", Type: graph.PortTypeString, Required: true},
			{Name: "gate_result_output", Type: graph.PortTypeString, Required: true},
		},
	)
	node.RegisterPorts("context",
		[]graph.Port{
			{Name: "files", Type: graph.PortTypeAny},
			{Name: "commands", Type: graph.PortTypeAny},
		},
		nil,
	)
	node.RegisterPorts("approval",
		[]graph.Port{{Name: "prompt", Type: graph.PortTypeString}},
		[]graph.Port{{Name: "approval_status", Type: graph.PortTypeString, Required: true}},
	)
	node.RegisterPorts("iteration",
		[]graph.Port{
			{Name: "items", Type: graph.PortTypeArray, Required: true},
			{Name: "body_script", Type: graph.PortTypeString, Required: true},
		},
		[]graph.Port{{Name: "iteration_results", Type: graph.PortTypeArray, Required: true}},
	)
}
