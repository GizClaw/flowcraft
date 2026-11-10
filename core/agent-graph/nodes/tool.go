package nodes

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// Tool returns the "tool" node kind: it executes the pending tool calls.
//
// config:
//
//	messages: state key holding the transcript (type messages), required
//	pending:  state key holding the calls (type json), required
//
// It appends one tool message with every result and clears the pending list, so a
// cycle back to the model reads both sides of the exchange. The messages key must
// merge as append: the node returns only this step's message, not the transcript.
//
// A tool cannot pause the run: the node has no translation from a tool result to
// graph.Suspend, and executing a batch atomically is what keeps the transcript
// consistent with the pending list.
type toolKind struct{ rt *runtime.Runtime }

type toolConfig struct {
	Messages string `json:"messages"`
	Pending  string `json:"pending"`
}

// DecodeConfig parses the node's config at Build.
func (*toolKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return decodeToolConfig(raw)
}

func decodeToolConfig(raw json.RawMessage) (toolConfig, error) {
	var config toolConfig
	if err := graph.DecodeConfig(raw, &config); err != nil {
		return toolConfig{}, err
	}
	return config, nil
}

// Tool builds the tool kind over a runtime.
func Tool(rt *runtime.Runtime) graph.NodeKind { return &toolKind{rt: rt} }

func (*toolKind) Name() string { return "tool" }

func (t *toolKind) Invoke(ctx context.Context, in graph.Invoke) (graph.NodeStream, error) {
	config, err := graph.ConfigOf(in, decodeToolConfig)
	if err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	if strings.TrimSpace(config.Messages) == "" || strings.TrimSpace(config.Pending) == "" {
		return nil, fmt.Errorf("tool: config.messages and config.pending are required")
	}
	raw, err := in.State(config.Pending).JSON()
	if err != nil {
		return nil, fmt.Errorf("tool: state %q: %w", config.Pending, err)
	}
	var calls []message.ToolCall
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &calls); err != nil {
			return nil, fmt.Errorf("tool: state %q: %w", config.Pending, err)
		}
	}
	if len(calls) == 0 {
		// Nothing to do: a node that finds no work succeeds without writing, so a
		// cycle can route on the state instead of on an error.
		return graph.Static(graph.Result{}), nil
	}

	if _, err := in.State(config.Messages).Messages(); err != nil {
		return nil, fmt.Errorf("tool: state %q: %w", config.Messages, err)
	}
	// The run's projection goes on the context for the duration of the calls:
	// tool_search resolves it from there and the assembly recorder feeds each
	// executed call back into it.
	view, ok := runtime.CatalogViewFrom(ctx)
	if !ok {
		view = t.rt.NewCatalogView()
	}
	results := t.rt.Assembly().ExecuteAll(engine.WithCatalogView(ctx, view), calls)
	return graph.Static(graph.Result{State: []graph.StateUpdate{
		{Key: config.Messages, Value: graph.MessagesValue(graph.MessagesFromSlice([]message.Message{toolMessage(results)}))},
		{Key: config.Pending, Value: graph.JSON([]byte("[]"))},
	}}), nil
}

// toolMessage wraps results into the one turn a model reads as tool output.
func toolMessage(results []message.ToolResult) message.Message {
	parts := make([]message.Part, 0, len(results))
	for _, result := range results {
		parts = append(parts, message.ToolResultPart{Result: result})
	}
	return message.Message{Role: message.RoleTool, Content: message.Content{Parts: parts}}
}
