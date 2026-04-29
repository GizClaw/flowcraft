package graph

import (
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// PortType identifies the data type of a node port.
type PortType string

const (
	PortTypeString   PortType = "string"
	PortTypeInteger  PortType = "integer"
	PortTypeFloat    PortType = "float"
	PortTypeBool     PortType = "bool"
	PortTypeArray    PortType = "array"
	PortTypeObject   PortType = "object"
	PortTypeMessages PortType = "messages"
	PortTypeUsage    PortType = "usage"
	PortTypeAny      PortType = "any"
)

// Port declares a typed input or output slot on a node.
type Port struct {
	Name     string   `json:"name"`
	Type     PortType `json:"type"`
	Required bool     `json:"required"`
	Desc     string   `json:"desc,omitempty"`
}

// IsCompatible checks if an output port type can feed into an input port type.
func IsCompatible(output, input PortType) bool {
	if output == PortTypeAny || input == PortTypeAny {
		return true
	}
	if output == input {
		return true
	}
	if output == PortTypeInteger && input == PortTypeFloat {
		return true
	}
	return false
}

// ValidateInputs checks that all required input ports have corresponding Board variables.
func ValidateInputs(b *Board, node PortDeclarable) error {
	return ValidateInputsWithConfig(b, node, nil)
}

// ValidateInputsWithConfig checks that all required input ports are satisfied,
// either by a board variable, a non-empty main channel (for message ports), or
// a value supplied directly via the node's resolved config map.
func ValidateInputsWithConfig(b *Board, node PortDeclarable, config map[string]any) error {
	for _, p := range node.InputPorts() {
		if !p.Required {
			continue
		}
		if _, ok := b.GetVar(p.Name); ok {
			continue
		}
		if (p.Name == VarMessages || p.Type == PortTypeMessages) && len(b.Channel(MainChannel)) > 0 {
			continue
		}
		if config != nil {
			if _, ok := config[p.Name]; ok {
				continue
			}
		}
		return errdefs.Validationf("missing required input port %q for node", p.Name)
	}
	return nil
}

// ValidateOutputs checks that all required output ports have been written to the Board.
func ValidateOutputs(b *Board, node PortDeclarable) error {
	for _, p := range node.OutputPorts() {
		if p.Required {
			if _, ok := b.GetVar(p.Name); !ok {
				return errdefs.Validationf("missing required output port %q from node", p.Name)
			}
		}
	}
	return nil
}
