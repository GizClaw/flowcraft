package graph

import (
	"bytes"
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// ---------- Node behaviour ----------

// RoleKind classifies a declared node I/O slot.
type RoleKind string

const (
	// RoleVar is an untyped board variable (Board.GetVar/SetVar).
	RoleVar RoleKind = "var"

	// RoleMessages is a typed message channel (Board.Channel).
	RoleMessages RoleKind = "messages"
)

// Role declares one I/O slot of a node type.
//
// The slot's board key is resolved once at [Build] time from exactly
// one of two sources:
//
//   - Name — a static key baked into the node type (e.g. a script
//     node always writing "script_result");
//   - ConfigKey — the name of a string field inside the node's own
//     config carrying the key per graph (e.g. an LLM node whose
//     "messages_channel" config field names the channel it consumes).
//
// Exactly one of the two must be set. A ConfigKey-bound value must be
// a compile-time constant: a "${board.*}" reference in the bound field
// is rejected at Build, since dynamic keys would defeat static
// validation.
//
// For RoleMessages an empty or absent config value means the main
// channel (agent.MainChannel). For RoleVar an empty key is an error.
//
// Required slots are enforced at invocation: a required RoleVar read
// must be present on the board before the handler runs; a required
// write must be present after it returns. For RoleMessages, "present"
// means non-empty (reads) resp. grown since invocation (writes).
// Missing optional slots are skipped silently.
type Role struct {
	Kind      RoleKind
	Name      string
	ConfigKey string
	Required  bool
}

// Meta is the static descriptor of a node type, supplied once at
// registration and shared by every graph that references the type.
type Meta struct {
	// Desc is a short human-readable description for docs/tooling.
	Desc string

	// Reads declares the board slots the handler consumes.
	Reads []Role

	// Writes declares the board slots the handler produces.
	Writes []Role
}

// NodeType binds a node type to its behaviour.
//
// C is the node type's config struct. The kernel treats config as
// opaque raw JSON all the way to invocation: "${board.*}" references
// are resolved first, then Decode produces the typed value the Handler
// receives. Handlers are plain functions — no lifecycle, no shared
// mutable state — so one registration serves any number of graphs and
// concurrent runs.
type NodeType[C any] struct {
	Meta Meta

	// Decode converts the (reference-resolved) raw config into C.
	// When nil, [DecodeConfig] is used. Supply a custom Decode for
	// defaults, normalisation, or validation beyond field names.
	Decode func(json.RawMessage) (C, error)

	// Handler is the node's behaviour. cfg is per-invocation data —
	// board references have already been resolved into concrete
	// values — so handlers can stay stateless.
	Handler func(ctx ExecutionContext, board *agent.Board, cfg C) error
}

// DecodeConfig is the default [NodeType.Decode]: strict JSON decoding
// (unknown fields rejected) into C.
func DecodeConfig[C any](raw json.RawMessage) (C, error) {
	var cfg C
	if len(raw) == 0 {
		return cfg, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return cfg, errdefs.Validationf("invalid node config: %v", err)
	}
	return cfg, nil
}

// ExecutionContext is the per-node, per-run invocation context handed
// to a [NodeType] handler.
type ExecutionContext struct {
	// Context carries the run's deadline and cancellation. Handlers
	// should thread it through all blocking calls.
	Context context.Context

	// Host is the run's host: event publishing, cooperative
	// interrupts, user prompts, checkpoints, usage reporting.
	Host agent.Host

	// NodeID is the id of the node being invoked, from the graph
	// definition.
	NodeID string

	// GraphID identifies the graph being executed — the
	// [GraphDefinition] name. Together with NodeID it locates the
	// invocation: which graph, which node. The kernel stamps it onto
	// envelope headers (event.HeaderGraphID).
	GraphID string

	// RunInfo is the node-facing projection of the current run:
	// identity, attributes, and tool policy.
	RunInfo agent.RunInfo
}

// EmitStreamDelta publishes a stream delta on this node's behalf.
//
// The kernel mints the envelope — subject
// agent.SubjectStreamDelta(runID, stepActor), NodeID/AgentID/RunID
// headers — and forwards it to Host.Publish, so node plugins never
// assemble subjects or envelopes themselves. A nil Host (tests) makes
// Emit a no-op.
func (ec ExecutionContext) EmitStreamDelta(delta agent.StreamDeltaPayload) error {
	return publishStreamDelta(ec.Context, ec.Host, ec.RunInfo, ec.GraphID, ec.NodeID, delta)
}

// ---------- role resolution (build time) ----------

// resolvedRole is a Role with its board key statically resolved.
type resolvedRole struct {
	Kind     RoleKind
	Key      string
	Required bool
}

// resolve converts a declared Role into a resolvedRole using the
// node's raw config. present is false when a ConfigKey-bound optional
// role's field is absent from the config.
func (r Role) resolve(typeName string, raw json.RawMessage) (rr resolvedRole, present bool, err error) {
	key := r.Name
	if r.ConfigKey != "" {
		var fields map[string]json.RawMessage
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &fields); err != nil {
				return rr, false, errdefs.Validationf(
					"node type %q: config is not a JSON object: %v", typeName, err)
			}
		}
		v, ok := fields[r.ConfigKey]
		if !ok {
			if r.Required {
				return rr, false, errdefs.Validationf(
					"node type %q: required role key field %q missing from config",
					typeName, r.ConfigKey)
			}
			return rr, false, nil
		}
		if err := json.Unmarshal(v, &key); err != nil {
			return rr, false, errdefs.Validationf(
				"node type %q: role key field %q must be a string: %v",
				typeName, r.ConfigKey, err)
		}
		if ContainsRef(key) {
			return rr, false, errdefs.Validationf(
				"node type %q: role key field %q must be a constant, got reference %q",
				typeName, r.ConfigKey, key)
		}
	}
	if r.Kind == RoleMessages && key == "" {
		key = agent.MainChannel
	}
	if r.Kind == RoleVar && key == "" {
		return rr, false, errdefs.Validationf(
			"node type %q: var role resolves to an empty key", typeName)
	}
	return resolvedRole{Kind: r.Kind, Key: key, Required: r.Required}, true, nil
}
