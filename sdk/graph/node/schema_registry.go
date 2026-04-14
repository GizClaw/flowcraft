package node

import (
	"sync"

	"github.com/GizClaw/flowcraft/sdk/graph"
)

// NodeSchema describes a node type for dynamic frontend rendering.
type NodeSchema struct {
	Type        string        `json:"type"`
	Label       string        `json:"label"`
	Icon        string        `json:"icon"`
	Color       string        `json:"color"`
	Category    string        `json:"category"`
	Description string        `json:"description"`
	Fields      []FieldSchema `json:"fields"`
	InputPorts  []PortSchema  `json:"input_ports,omitempty"`
	OutputPorts []PortSchema  `json:"output_ports,omitempty"`
	Deprecated  bool          `json:"deprecated,omitempty"`
	Runtime     *RuntimeSpec  `json:"runtime,omitempty"`
}

// RuntimeSpec describes the runtime behavior of a node type — what board
// variables it reads/writes, what condition variables it produces for
// downstream edges, and important behavioral notes. This information is
// returned by the schema(action=node_usage) tool to help the Builder understand how to
// correctly wire nodes together.
type RuntimeSpec struct {
	BoardWrites []BoardVarSpec `json:"board_writes,omitempty"`
	BoardReads  []BoardVarSpec `json:"board_reads,omitempty"`
	EdgeVars    []BoardVarSpec `json:"edge_vars,omitempty"`
	Notes       []string       `json:"notes,omitempty"`
}

// BoardVarSpec describes a single board variable that a node reads or writes.
type BoardVarSpec struct {
	Key       string `json:"key"`
	Type      string `json:"type"`
	Desc      string `json:"desc"`
	Condition string `json:"condition,omitempty"`
}

// FieldSchema describes a single configurable field of a node type.
type FieldSchema struct {
	Key          string         `json:"key"`
	Label        string         `json:"label"`
	Type         string         `json:"type"`
	Required     bool           `json:"required,omitempty"`
	Placeholder  string         `json:"placeholder,omitempty"`
	DefaultValue any            `json:"default_value,omitempty"`
	Options      []SelectOption `json:"options,omitempty"`
}

// PortSchema describes a typed port for frontend rendering.
type PortSchema struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Required    bool   `json:"required,omitempty"`
	Description string `json:"description,omitempty"`
}

// SelectOption is a label-value pair for dropdown fields.
type SelectOption struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// SchemaRegistry is a thread-safe registry mapping node type strings to their schemas.
type SchemaRegistry struct {
	mu      sync.RWMutex
	schemas map[string]NodeSchema
	order   []string
}

func NewSchemaRegistry() *SchemaRegistry {
	return &SchemaRegistry{schemas: make(map[string]NodeSchema)}
}

func (r *SchemaRegistry) Register(schema NodeSchema) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.schemas[schema.Type]; !exists {
		r.order = append(r.order, schema.Type)
	}
	r.schemas[schema.Type] = schema
}

func (r *SchemaRegistry) RegisterMany(schemas []NodeSchema) {
	for _, s := range schemas {
		r.Register(s)
	}
}

// Unregister removes a node schema by type name.
func (r *SchemaRegistry) Unregister(nodeType string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.schemas[nodeType]; !ok {
		return
	}
	delete(r.schemas, nodeType)
	for i, t := range r.order {
		if t == nodeType {
			r.order = append(r.order[:i], r.order[i+1:]...)
			break
		}
	}
}

func (r *SchemaRegistry) Get(nodeType string) (NodeSchema, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	s, ok := r.schemas[nodeType]
	return s, ok
}

func (r *SchemaRegistry) All() []NodeSchema {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]NodeSchema, 0, len(r.order))
	for _, t := range r.order {
		if s, ok := r.schemas[t]; ok {
			out = append(out, s)
		}
	}
	return out
}

func (r *SchemaRegistry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.schemas)
}

// --- Package-level default schema registry ---

var (
	defaultSchemasMu sync.RWMutex
	defaultSchemas   []NodeSchema
)

func RegisterDefaultSchema(schema NodeSchema) {
	defaultSchemasMu.Lock()
	for i, existing := range defaultSchemas {
		if existing.Type == schema.Type {
			defaultSchemas[i] = schema
			defaultSchemasMu.Unlock()
			return
		}
	}
	defaultSchemas = append(defaultSchemas, schema)
	defaultSchemasMu.Unlock()
}

func RegisterBuiltinSchemas(reg *SchemaRegistry) {
	reg.Register(NodeSchema{
		Type:        "__end__",
		Label:       "End",
		Icon:        "CircleStop",
		Color:       "gray",
		Category:    "control",
		Description: "Marks the end of graph execution",
	})

	defaultSchemasMu.RLock()
	schemas := make([]NodeSchema, len(defaultSchemas))
	copy(schemas, defaultSchemas)
	defaultSchemasMu.RUnlock()

	for _, s := range schemas {
		reg.Register(s)
	}
}

// PortsForType returns the graph.Port slices for a registered node type,
// derived from the schema's PortSchema definitions. This is the single source
// of truth — jsnode and other implementations should call this instead of
// hardcoding port definitions.
func PortsForType(nodeType string) (input, output []graph.Port) {
	defaultSchemasMu.RLock()
	defer defaultSchemasMu.RUnlock()
	for _, s := range defaultSchemas {
		if s.Type == nodeType {
			return convertPorts(s.InputPorts), convertPorts(s.OutputPorts)
		}
	}
	return nil, nil
}

func convertPorts(ps []PortSchema) []graph.Port {
	if len(ps) == 0 {
		return nil
	}
	out := make([]graph.Port, len(ps))
	for i, p := range ps {
		out[i] = graph.Port{
			Name:     p.Name,
			Type:     graph.PortType(p.Type),
			Required: p.Required,
			Desc:     p.Description,
		}
	}
	return out
}
