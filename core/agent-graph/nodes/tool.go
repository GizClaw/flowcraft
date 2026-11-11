package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
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
// Calls run in order, one start/result event pair each: the transcript stays
// consistent with the pending list, and a live observer sees which tool is
// running instead of a silent gap between two model rounds.
//
// A tool cannot pause the run: the node has no translation from a tool result
// to graph.Suspend.
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
	return &toolStream{
		rt:          t.rt,
		view:        view,
		calls:       calls,
		messagesKey: config.Messages,
		pendingKey:  config.Pending,
	}, nil
}

// toolStream executes the batch in order and reports each call.
type toolStream struct {
	rt          *runtime.Runtime
	view        engine.CatalogView
	calls       []message.ToolCall
	index       int
	pending     *message.ToolCall
	results     []message.ToolResult
	messagesKey string
	pendingKey  string
	result      graph.Result
	done        bool
}

func (s *toolStream) Next(ctx context.Context) (agent.Event, error) {
	if s.pending != nil {
		call := *s.pending
		s.pending = nil
		started := time.Now()
		result := s.rt.Assembly().Execute(engine.WithCatalogView(ctx, s.view), call)
		duration := time.Since(started)
		s.results = append(s.results, result)
		body, err := json.Marshal(result)
		if err != nil {
			return nil, err
		}
		return agent.ToolResult{
			Result:     result,
			ResultHash: hashBytes(body),
			Duration:   duration,
			ErrorKind:  toolErrorKind(result),
		}, nil
	}
	if s.index < len(s.calls) {
		call := s.calls[s.index]
		s.index++
		s.pending = &call
		body, err := json.Marshal(call)
		if err != nil {
			return nil, err
		}
		return agent.ToolCallStart{Call: call, ArgsHash: hashBytes(body)}, nil
	}
	msg := toolMessage(s.results)
	msg.Metadata = toolOrigin(s.calls)
	s.result = graph.Result{State: []graph.StateUpdate{
		{Key: s.messagesKey, Value: graph.MessagesValue(graph.MessagesFromSlice([]message.Message{msg}))},
		{Key: s.pendingKey, Value: graph.JSON([]byte("[]"))},
	}}
	s.done = true
	return nil, io.EOF
}

func (s *toolStream) Result() (graph.Result, error) {
	if !s.done {
		return graph.Result{}, fmt.Errorf("tool: result before the stream ended")
	}
	return s.result, nil
}

func (*toolStream) Close() error { return nil }

// toolErrorKind classifies a result for the trace. A tool reports failure
// through the result, not by failing the node.
func toolErrorKind(result message.ToolResult) string {
	if result.IsError {
		return "tool_error"
	}
	return ""
}

// toolOrigin tags the tool turn with where it came from: one name when the
// batch is uniform, "tool" when it is mixed.
func toolOrigin(calls []message.ToolCall) *message.Metadata {
	origin := "tool"
	if len(calls) > 0 {
		origin = "tool:" + calls[0].Name
		for _, call := range calls[1:] {
			if call.Name != calls[0].Name {
				origin = "tool"
				break
			}
		}
	}
	raw, err := json.Marshal(origin)
	if err != nil {
		return nil
	}
	return &message.Metadata{Attributes: map[string]json.RawMessage{"origin": raw}}
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// toolMessage wraps results into the one turn a model reads as tool output.
func toolMessage(results []message.ToolResult) message.Message {
	parts := make([]message.Part, 0, len(results))
	for _, result := range results {
		parts = append(parts, message.ToolResultPart{Result: result})
	}
	return message.Message{Role: message.RoleTool, Content: message.Content{Parts: parts}}
}
