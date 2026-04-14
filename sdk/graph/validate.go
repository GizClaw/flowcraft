package graph

import (
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// ValidateInputs checks that all required input ports have corresponding Board variables.
func ValidateInputs(b *Board, node PortDeclarable) error {
	return ValidateInputsWithConfig(b, node, nil)
}

// ValidateInputsWithConfig checks that all required input ports are satisfied.
func ValidateInputsWithConfig(b *Board, node PortDeclarable, config map[string]any) error {
	for _, p := range node.InputPorts() {
		if !p.Required {
			continue
		}
		if _, ok := b.GetVar(p.Name); ok {
			continue
		}
		if (p.Name == workflow.VarMessages || p.Type == PortTypeMessages) && len(b.Channel(workflow.MainChannel)) > 0 {
			continue
		}
		if config != nil {
			if _, ok := config[p.Name]; ok {
				continue
			}
		}
		return fmt.Errorf("missing required input port %q for node", p.Name)
	}
	return nil
}

// ValidateOutputs checks that all required output ports have been written to the Board.
func ValidateOutputs(b *Board, node PortDeclarable) error {
	for _, p := range node.OutputPorts() {
		if p.Required {
			if _, ok := b.GetVar(p.Name); !ok {
				return fmt.Errorf("missing required output port %q from node", p.Name)
			}
		}
	}
	return nil
}
