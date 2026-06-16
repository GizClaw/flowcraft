package scriptnode

import (
	"reflect"

	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/graph/node/scripts"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/bindings"
)

// builtinSpec is the graph-layer declaration for one embedded script-backed
// node. sdk/graph/node/scripts owns only embedded source loading; scriptnode
// owns the node type, port shape, and host capability policy that make that
// source usable as a graph node.
type builtinSpec struct {
	nodeType              string
	inputPorts            []graph.Port
	outputPorts           []graph.Port
	requiresCommandRunner func(config map[string]any) bool
}

func (s builtinSpec) Type() string {
	return s.nodeType
}

func (s builtinSpec) Source() (string, error) {
	return scripts.Get(s.nodeType)
}

func (s builtinSpec) Ports() (input, output []graph.Port) {
	return clonePorts(s.inputPorts), clonePorts(s.outputPorts)
}

func (s builtinSpec) needsCommandRunner(config map[string]any) bool {
	return s.requiresCommandRunner != nil && s.requiresCommandRunner(config)
}

func (s builtinSpec) defaultBridges(deps Deps) []script.BindingFunc {
	return []script.BindingFunc{bindings.NewFSBridge(deps.Workspace)}
}

var builtinCatalog = []builtinSpec{
	{
		nodeType:   "router",
		inputPorts: []graph.Port{{Name: "routes", Type: graph.PortTypeAny, Required: true}},
		outputPorts: []graph.Port{
			{Name: "route_target", Type: graph.PortTypeString, Required: true},
		},
	},
	{
		nodeType:    "ifelse",
		inputPorts:  []graph.Port{{Name: "conditions", Type: graph.PortTypeAny, Required: true}},
		outputPorts: []graph.Port{{Name: "branch_result", Type: graph.PortTypeString, Required: true}},
	},
	{
		nodeType:    "template",
		inputPorts:  []graph.Port{{Name: "template", Type: graph.PortTypeString, Required: true}},
		outputPorts: []graph.Port{{Name: "template_output", Type: graph.PortTypeString, Required: true}},
	},
	{
		nodeType: "answer",
		inputPorts: []graph.Port{
			{Name: "template", Type: graph.PortTypeString},
			{Name: "keys", Type: graph.PortTypeAny},
		},
		outputPorts: []graph.Port{{Name: "answer", Type: graph.PortTypeString, Required: true}},
	},
	{
		nodeType:    "assigner",
		inputPorts:  []graph.Port{{Name: "assignments", Type: graph.PortTypeAny, Required: true}},
		outputPorts: nil,
	},
	{
		nodeType:   "loopguard",
		inputPorts: nil,
		outputPorts: []graph.Port{
			{Name: "loop_count", Type: graph.PortTypeInteger, Required: true},
			{Name: "loop_count_exceeded", Type: graph.PortTypeBool, Required: true},
		},
	},
	{
		nodeType:    "aggregator",
		inputPorts:  []graph.Port{{Name: "input_keys", Type: graph.PortTypeAny, Required: true}},
		outputPorts: []graph.Port{{Name: "aggregated", Type: graph.PortTypeAny}},
	},
	{
		nodeType:   "gate",
		inputPorts: []graph.Port{{Name: "commands", Type: graph.PortTypeAny, Required: true}},
		outputPorts: []graph.Port{
			{Name: "gate_result", Type: graph.PortTypeString, Required: true},
			{Name: "gate_result_output", Type: graph.PortTypeString, Required: true},
		},
		requiresCommandRunner: configCommandsNonEmpty,
	},
	{
		nodeType: "context",
		inputPorts: []graph.Port{
			{Name: "files", Type: graph.PortTypeAny},
			{Name: "commands", Type: graph.PortTypeAny},
		},
		outputPorts:           nil,
		requiresCommandRunner: configCommandsNonEmpty,
	},
	{
		nodeType:    "approval",
		inputPorts:  []graph.Port{{Name: "prompt", Type: graph.PortTypeString}},
		outputPorts: []graph.Port{{Name: "approval_status", Type: graph.PortTypeString, Required: true}},
	},
	{
		nodeType: "iteration",
		inputPorts: []graph.Port{
			{Name: "items", Type: graph.PortTypeArray, Required: true},
			{Name: "body_script", Type: graph.PortTypeString, Required: true},
		},
		outputPorts: []graph.Port{{Name: "iteration_results", Type: graph.PortTypeArray, Required: true}},
	},
}

var builtinCatalogByType = func() map[string]builtinSpec {
	byType := make(map[string]builtinSpec, len(builtinCatalog))
	for _, spec := range builtinCatalog {
		byType[spec.nodeType] = spec
	}
	return byType
}()

func builtinSpecForType(nodeType string) (builtinSpec, bool) {
	spec, ok := builtinCatalogByType[nodeType]
	return spec, ok
}

func clonePorts(ports []graph.Port) []graph.Port {
	if ports == nil {
		return nil
	}
	return append([]graph.Port(nil), ports...)
}

// configCommandsNonEmpty mirrors the command-bridge behavior of context.js and
// gate.js: the shell bridge is required only when config.commands is present
// and non-empty.
func configCommandsNonEmpty(config map[string]any) bool {
	if config == nil {
		return false
	}
	v, ok := config["commands"]
	if !ok || v == nil {
		return false
	}
	rv := reflect.ValueOf(v)
	switch rv.Kind() {
	case reflect.Array, reflect.Chan, reflect.Map, reflect.Slice, reflect.String:
		return rv.Len() > 0
	default:
		return true
	}
}
