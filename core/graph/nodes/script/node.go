package script

import (
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent/bindings"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/workspace"
)

// ScriptConfig is the config of the "script" node type. Decoding is
// strict: unknown top-level fields are typos, not script config.
type ScriptConfig struct {
	// Runtime selects the wired agent.ScriptRuntime by name
	// (e.g. "js", "lua").
	Runtime string `json:"runtime"`
	// Name labels the execution for errors and runtime pooling;
	// defaults to the node ID.
	Name string `json:"name,omitempty"`
	// Source is the inline script source.
	Source string `json:"source"`
	// Config becomes the script's config global. Values may carry
	// board references — the kernel resolves ${board.*} before the
	// node ever sees them.
	Config map[string]any `json:"config,omitempty"`
}

// ScriptNodeDeps wires the script node's collaborators. Runtimes is
// the only required entry; a nil dispatcher/router simply disables the
// corresponding global (calls on it fail closed).
type ScriptNodeDeps struct {
	Runtimes map[string]agent.ScriptRuntime

	// Tools global: dispatcher executes, catalog answers lookups;
	// options carry the allow-list policy (e.g.
	// bindings.WithAllowedToolNames).
	ToolDispatcher tool.Dispatcher
	ToolCatalog    tool.Catalog
	ToolOptions    []bindings.ToolBridgeOption

	// Inference global: runtime serves generate/stream, router serves
	// route/routeStream; options register extension decoders.
	InferenceAssembly *inference.Assembly
	InferenceRouter   *route.Router
	InferenceOptions  []bindings.InferenceBridgeOption

	// Workspace powers the "fs" global (fs.read/write/exists/delete).
	// Nil leaves the global unregistered.
	Workspace workspace.Workspace

	// CommandRunner powers the "shell" global (shell.exec); options
	// carry sandbox policy. Nil leaves the global unregistered.
	CommandRunner sandbox.Runner
	ShellOptions  []bindings.ShellOption
}

// NewNode returns the "script" node type. Each invocation assembles a
// fresh ScriptEnv with the standard bridges — board (direct
// read/write), expr, host (publish/emit/interrupt/askUser/usage), run
// (ambient identity), tools, inference, node (graph identity), stream
// (event subscriptions), parallel (branch cancellation) — plus the
// opt-in fs/shell globals when Workspace/CommandRunner are wired —
// executes the inline source, and maps control signals to Go errors
// via agent.SignalToError.
//
// Board access is dynamic by nature (the script decides what it reads
// and writes), so the type declares no static roles: analysis treats
// script nodes as opaque.
func NewNode(deps ScriptNodeDeps) graph.NodeType[ScriptConfig] {
	return graph.NodeType[ScriptConfig]{
		Meta: graph.Meta{
			Desc: "run an embedded script with host bindings (board/host/run/tools/inference)",
		},
		Decode: decodeScriptConfig,
		Handler: func(ec graph.ExecutionContext, board *agent.Board, cfg ScriptConfig) error {
			runtime, ok := deps.Runtimes[cfg.Runtime]
			if !ok || runtime == nil {
				return errdefs.NotAvailablef("script node: runtime %q is not wired", cfg.Runtime)
			}
			name := cfg.Name
			if name == "" {
				name = ec.NodeID
			}
			info, _ := agent.RunInfoFromContext(ec.Context)

			// Subscriptions opened mid-script are bound to the script's
			// lifetime: the cleanup registry rides the context the
			// bridges bind against and Exec receives, then flushes when
			// Exec returns — subscriptions never outlive the node
			// invocation.
			execCtx, cleanups := withStreamCleanup(ec.Context)
			defer cleanups.flush()

			env := bindings.NewEnvBuilder(cfg.Config).
				Add(
					bindings.NewBoardBridge(board),
					bindings.NewExprBridge(),
					bindings.NewHostBridge(ec.Host, name, scriptEmitter{ec}),
					bindings.NewRunInfoBridge(),
					bindings.NewToolBridge(deps.ToolDispatcher, deps.ToolCatalog, deps.ToolOptions...),
					bindings.NewInferenceBridge(deps.InferenceAssembly, deps.InferenceRouter, deps.InferenceOptions...),
					newNodeBridge(ec.NodeID, ec.NodeType),
					newStreamBridge(info.RunID, ec.Host),
					newParallelBridge(),
				).
				AddIf(deps.Workspace != nil, bindings.NewFSBridge(deps.Workspace)).
				AddIf(deps.CommandRunner != nil, bindings.NewShellBridge(deps.CommandRunner, deps.ShellOptions...)).
				AddLate(bindings.NewRuntimeBridge(runtime)).
				Build(execCtx)

			sig, err := runtime.Exec(execCtx, name, cfg.Source, env)
			if err != nil {
				return err
			}
			return agent.SignalToError(sig)
		},
	}
}

func Register(reg *graph.Registry, deps ScriptNodeDeps) error {
	return graph.RegisterType(reg, "script", NewNode(deps))
}

// decodeScriptConfig strict-decodes the node config and enforces the
// two required fields.
// Register registers the "script" node type into reg.
func decodeScriptConfig(raw json.RawMessage) (ScriptConfig, error) {
	cfg, err := graph.DecodeConfig[ScriptConfig](raw)
	if err != nil {
		return ScriptConfig{}, err
	}
	if cfg.Runtime == "" {
		return ScriptConfig{}, errdefs.Validationf("script node: runtime is required")
	}
	if cfg.Source == "" {
		return ScriptConfig{}, errdefs.Validationf("script node: source is required")
	}
	return cfg, nil
}

// scriptEmitter adapts graph's per-node stream-delta channel to the
// host bridge's StreamEmitter. The bridge's emit is fire-and-forget,
// so publish failures are dropped here — matching the emitter's void
// contract.
type scriptEmitter struct{ ec graph.ExecutionContext }

func (e scriptEmitter) Emit(eventType string, payload any) {
	delta := agent.StreamDeltaPayload{}
	switch eventType {
	case "token":
		delta = agent.StreamDeltaPayload{
			Type: agent.StreamDeltaPart,
			Part: message.TextPart{Text: stringifyPayload(payload)},
		}
	case "tool_call":
		if call, ok := payloadToToolCall(payload); ok {
			delta = agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: message.ToolCallPart{Call: call},
			}
		}
	case "tool_result":
		if result, ok := payloadToToolResult(payload); ok {
			delta = agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: message.ToolResultPart{Result: result},
			}
		}
	case "part":
		// Generic part event: the payload is a canonical part wire
		// object (e.g. {"type":"image","source":...}), so scripts can
		// emit any message.Part kind.
		if part, ok := payloadToPart(payload); ok {
			delta = agent.StreamDeltaPayload{
				Type: agent.StreamDeltaPart,
				Part: part,
			}
		}
	default:
		// Forward-compatible event types pass through unchanged.
		delta = agent.StreamDeltaPayload{Type: agent.StreamDeltaType(eventType)}
	}
	_ = e.ec.EmitStreamDelta(delta)
}

// payloadToPart decodes a canonical part wire object (a map or JSON
// string carrying a "type" discriminator) into a message.Part.
func payloadToPart(payload any) (message.Part, bool) {
	raw, ok := marshalPayload(payload)
	if !ok {
		return nil, false
	}
	part, err := message.UnmarshalPart(raw)
	if err != nil {
		return nil, false
	}
	return part, true
}

// stringifyPayload renders a string payload verbatim and any other
// value as its JSON text.
func stringifyPayload(payload any) string {
	if s, ok := payload.(string); ok {
		return s
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Sprint(payload)
	}
	return string(buf)
}

// payloadToToolCall decodes a script-supplied tool_call payload into a
// message.ToolCall. Accepted shapes: a JSON string or an object with
// id / name / arguments keys.
func payloadToToolCall(payload any) (message.ToolCall, bool) {
	raw, ok := marshalPayload(payload)
	if !ok {
		return message.ToolCall{}, false
	}
	var wire struct {
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return message.ToolCall{}, false
	}
	call := message.ToolCall{ID: wire.ID, Name: wire.Name, Arguments: wire.Arguments}
	if err := call.Validate(); err != nil {
		return message.ToolCall{}, false
	}
	return call, true
}

// payloadToToolResult decodes a script-supplied tool_result payload
// into a message.ToolResult. The script-facing field keeps the legacy
// tool_call_id name; the wire result type uses call_id.
func payloadToToolResult(payload any) (message.ToolResult, bool) {
	raw, ok := marshalPayload(payload)
	if !ok {
		return message.ToolResult{}, false
	}
	var wire struct {
		CallID  string `json:"tool_call_id"`
		Content string `json:"content"`
		IsError bool   `json:"is_error"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return message.ToolResult{}, false
	}
	result := message.ToolResult{CallID: wire.CallID, Content: wire.Content, IsError: wire.IsError}
	if err := result.Validate(); err != nil {
		return message.ToolResult{}, false
	}
	return result, true
}

func marshalPayload(payload any) ([]byte, bool) {
	switch v := payload.(type) {
	case string:
		return []byte(v), true
	case []byte:
		return v, true
	default:
		buf, err := json.Marshal(v)
		return buf, err == nil
	}
}
