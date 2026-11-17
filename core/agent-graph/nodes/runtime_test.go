package nodes_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/goccy/go-json"
)

// stubRuntime is a substrate a test writes in one screen: the five methods a node
// kind reads, over an assembly built from a source the test names. No sandbox, no
// file tree, no provider table - that assembly is the only thing between the kind
// and the tool it calls.
type stubRuntime struct {
	assembly *engine.Assembly
	provider inference.Provider
	model    model.ModelRef
}

var _ nodes.Runtime = (*stubRuntime)(nil)

func newStubRuntime(t *testing.T, provider inference.Generator, tools ...tool.Tool) *stubRuntime {
	t.Helper()
	assembly, err := engine.NewAssembly([]tool.Source{stubToolSource(tools)})
	if err != nil {
		t.Fatalf("engine.NewAssembly: %v", err)
	}
	return &stubRuntime{
		assembly: assembly,
		provider: inference.Provider{Generator: provider},
		model:    model.ModelRef{ID: model.ModelID{Provider: "stub", Name: "m"}},
	}
}

func (r *stubRuntime) NewCatalogView() engine.CatalogView { return r.assembly.NewCatalogView() }
func (r *stubRuntime) Assembly() *engine.Assembly         { return r.assembly }
func (r *stubRuntime) Provider() inference.Provider       { return r.provider }
func (*stubRuntime) Router() *route.Router                { return nil }
func (r *stubRuntime) DefaultModel() model.ModelRef       { return r.model }

// stubToolSource is the tools a test hands the stub assembly.
type stubToolSource []tool.Tool

func (s stubToolSource) Tools() []tool.Tool       { return s }
func (stubToolSource) LazyTools() []tool.LazyTool { return nil }

// TestModelRunsOverAStubRuntime builds the model kind from the interface alone,
// and reads back what it took from it: the default model a request that names none
// resolves to, and the generator the call goes through.
func TestModelRunsOverAStubRuntime(t *testing.T) {
	provider := &stubProvider{response: inference.GenerateResponse{
		Message: message.NewTextMessage(message.RoleAssistant, "answered"),
	}}
	rt := newStubRuntime(t, provider)
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "anything"),
		})),
	}

	stream, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","output":"answer"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)
	if text, err := updateFor(t, result, "answer").Text(); err != nil || text != "answered" {
		t.Fatalf("answer = %q, %v, want the stub's reply", text, err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("requests = %d, want the call to reach the stub provider", len(provider.requests))
	}
	if got := provider.requests[0].Model; got.ID.Provider != "stub" || got.ID.Name != "m" {
		t.Errorf("model = %+v, want the stub runtime's default", got)
	}
}

// TestToolRunsOverAStubRuntime dispatches a pending call through the stub
// assembly: the kind needs nothing but Assembly from the substrate to execute it.
func TestToolRunsOverAStubRuntime(t *testing.T) {
	echo := tool.FuncTool(
		message.ToolDefinition{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)},
		func(context.Context, string) (message.Content, error) {
			return message.NewTextContent("ok"), nil
		})
	rt := newStubRuntime(t, &stubProvider{}, echo)
	call, err := message.NewToolCall("call-1", "echo", map[string]any{})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	pending, err := json.Marshal([]message.ToolCall{call})
	if err != nil {
		t.Fatalf("encode pending: %v", err)
	}
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "say ok"),
		})),
		"pending": graph.JSON(pending),
	}

	stream, err := nodes.Tool(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","pending":"pending"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)
	transcript, err := updateFor(t, result, "messages").Messages()
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	if transcript.Len() != 1 || transcript.Slice()[0].Role != message.RoleTool {
		t.Fatalf("delta = %+v, want the one tool message", transcript.Slice())
	}
	parts := transcript.Slice()[0].Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the one result", len(parts))
	}
	part, ok := parts[0].(message.ToolResultPart)
	if !ok {
		t.Fatalf("part = %T, want a tool result", parts[0])
	}
	if text := part.Result.Content.Text(); text != "ok" || part.Result.IsError {
		t.Errorf("tool result = %q (error %t), want the stub tool's answer", text, part.Result.IsError)
	}
}
