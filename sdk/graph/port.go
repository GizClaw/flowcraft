package graph

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
