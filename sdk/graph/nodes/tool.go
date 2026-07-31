package nodes

import (
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// ToolConfig is the config of the "tool" node type.
type ToolConfig struct {
	// MessagesChannel names the channel whose tail holds the pending
	// tool calls; empty means the main channel. The executed results
	// are appended to the same channel as one role=tool message —
	// which is again a valid tail for the inference node.
	MessagesChannel string `json:"messages_channel,omitempty"`

	// ResultsKey, when set, receives the raw []tool.Result for
	// inspection by downstream nodes or conditions.
	ResultsKey string `json:"results_key,omitempty"`
}

// Tool returns the "tool" node type: it reads the tool_call parts off
// the channel's tail assistant message, executes the whole batch
// through the dispatcher (model-issued call ids preserved, so the
// provider can pair results on the next turn), and appends the
// results as one role=tool message. Allow-listing and policy live in
// the dispatcher's middleware chain, not in the node.
func Tool(dispatcher tool.Dispatcher) graph.NodeType[ToolConfig] {
	return graph.NodeType[ToolConfig]{
		Meta: graph.Meta{
			Desc: "execute the channel tail's tool calls as one batch, append one tool message",
			Reads: []graph.Role{
				{Kind: graph.RoleMessages, ConfigKey: "messages_channel"},
			},
			Writes: []graph.Role{
				{Kind: graph.RoleMessages, ConfigKey: "messages_channel"},
				{Kind: graph.RoleVar, ConfigKey: "results_key"},
			},
		},
		Handler: func(ec graph.ExecutionContext, board *agent.Board, cfg ToolConfig) error {
			if dispatcher == nil {
				return errdefs.NotAvailablef("tool node: no dispatcher wired")
			}
			channel := cfg.MessagesChannel
			if channel == "" {
				channel = agent.MainChannel
			}
			messages := board.Channel(channel)
			if len(messages) == 0 {
				return errdefs.Validationf("tool node: messages channel %q is empty", channel)
			}
			last := messages[len(messages)-1]
			if last.Role != inference.RoleAssistant {
				return errdefs.Validationf(
					"tool node: last message on channel %q must have role assistant, got %q",
					channel, last.Role)
			}

			var calls []tool.Call
			for _, part := range last.Content.Parts {
				if call, ok := part.(inference.ToolCallPart); ok {
					calls = append(calls, call.Call)
				}
			}
			if len(calls) == 0 {
				return errdefs.Validationf("tool node: last message on channel %q carries no tool calls", channel)
			}

			results := dispatcher.ExecuteAll(ec.Context, calls)
			parts := make([]inference.Part, len(results))
			for i, result := range results {
				parts[i] = inference.ToolResultPart{Result: result}
			}
			board.AppendChannelMessage(channel, inference.Message{
				Role:    inference.RoleTool,
				Content: inference.Content{Parts: parts},
			})
			if cfg.ResultsKey != "" {
				board.SetVar(cfg.ResultsKey, results)
			}
			return nil
		},
	}
}

// RegisterTool registers the "tool" node type into reg.
func RegisterTool(reg *graph.Registry, dispatcher tool.Dispatcher) error {
	return graph.RegisterType(reg, "tool", Tool(dispatcher))
}
