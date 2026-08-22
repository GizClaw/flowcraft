package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/graph"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/inference/route"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The node tests run full Build→Execute cycles over the canned
// provider in inferencetest: real Runtime/Router/tool.Registry, no
// hand-rolled fakes beyond the event-capturing host.

// captureHost records published envelopes so stream-delta assertions
// can see what subscribers would have seen.
type captureHost struct {
	agent.NoopHost
	mu        sync.Mutex
	envelopes []event.Envelope
	usages    []inference.Usage
}

func (h *captureHost) Publish(_ context.Context, env event.Envelope) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.envelopes = append(h.envelopes, env)
	return nil
}

func (h *captureHost) ReportUsage(_ context.Context, usage inference.Usage) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.usages = append(h.usages, usage)
	return nil
}

func (h *captureHost) published(subject event.Subject) []event.Envelope {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []event.Envelope
	for _, env := range h.envelopes {
		if env.Subject == subject {
			out = append(out, env)
		}
	}
	return out
}

func mustConfig(t *testing.T, v any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return raw
}

func singleNodeGraph(t *testing.T, reg *graph.Registry, nodeType string, config any) *graph.Graph {
	t.Helper()
	g, err := graph.Build(&graph.GraphDefinition{
		Name:  "test-graph",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{ID: "n", Type: nodeType, Config: mustConfig(t, config)}},
	}, reg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func executeGraph(t *testing.T, g *graph.Graph, host agent.Host, board *agent.Board) error {
	t.Helper()
	_, err := g.Execute(context.Background(),
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		host, board)
	return err
}

func userBoard() *agent.Board {
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleUser, "hi"))
	return board
}

// nodesToolSource adapts plain tools to a tool.Source for test
// registries.
type nodesToolSource struct {
	tools []tool.Tool
}

func (s nodesToolSource) Tools() []tool.Tool         { return s.tools }
func (s nodesToolSource) LazyTools() []tool.LazyTool { return nil }

func toolCatalog(t *testing.T, tools ...tool.Tool) *tool.Registry {
	t.Helper()
	reg, err := tool.NewRegistry([]tool.Source{nodesToolSource{tools: tools}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	return reg
}

// stubTool is a minimal catalog tool for node tests.
type stubTool struct {
	name, desc string
}

func (t stubTool) Definition() message.ToolDefinition {
	return message.DefineSchema(t.name, t.desc).Build()
}

func (stubTool) Execute(context.Context, string) (string, error) { return "ok", nil }

func inferenceRegistry(t *testing.T, deps InferenceNodeDeps) *graph.Registry {
	t.Helper()
	reg := graph.NewRegistry()
	if err := graph.RegisterType(reg, "inference", Inference(deps)); err != nil {
		t.Fatalf("register inference: %v", err)
	}
	return reg
}

func fakeRouter(t *testing.T, runtime *inference.Assembly) *route.Router {
	t.Helper()
	router, err := route.New(runtime, route.Selectors{
		Generate: inferencetest.StaticGenerateSelector(inferencetest.DefaultFakeModel),
	})
	if err != nil {
		t.Fatalf("route.New: %v", err)
	}
	return router
}

func TestInferenceNode_Unary_WritesMessageAndVars(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:          ptr(inferencetest.DefaultFakeModel),
		OutputKey:      "answer",
		UsageKey:       "usage",
		ToolPendingKey: "tool_pending",
	})
	board := userBoard()
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 || msgs[1].Role != message.RoleAssistant {
		t.Fatalf("channel = %+v, want user + assistant", msgs)
	}
	if text, ok := msgs[1].Content.Parts[0].(message.TextPart); !ok || text.Text != "ok" {
		t.Fatalf("assistant message = %+v, want text %q", msgs[1].Content.Parts[0], "ok")
	}
	if v, ok := board.GetVar("answer"); !ok {
		t.Fatal("output_key var missing")
	} else if msg, ok := v.(message.Message); !ok || msg.Role != message.RoleAssistant {
		t.Fatalf("output var = %T, want message.Message", v)
	}
	if pending, ok := board.GetVar("tool_pending"); !ok || pending != false {
		t.Fatalf("tool_pending = %v, want false", pending)
	}
	if _, ok := board.GetVar("usage"); !ok {
		t.Fatal("usage_key var missing")
	}

	// The channel tail became the input; everything before it the
	// context. Here: no context, one user input.
	req := fake.LastRequest()
	if len(req.Context) != 0 || req.Input.Role != inference.InputRoleUser {
		t.Fatalf("request = %+v, want one user input", req)
	}
	if text, ok := req.Input.Content.Parts[0].(message.TextPart); !ok || text.Text != "hi" {
		t.Fatalf("input = %+v, want text %q", req.Input.Content.Parts[0], "hi")
	}
	if req.Input.Content.Intent.Text == nil {
		t.Fatal("text intent missing")
	}
}

func TestInferenceNode_SystemPromptPrepended(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:        ptr(inferencetest.DefaultFakeModel),
		SystemPrompt: "be terse",
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ctx := fake.LastRequest().Context
	if len(ctx) != 1 || ctx[0].Role != message.RoleSystem {
		t.Fatalf("context = %+v, want one system message", ctx)
	}
	if text, ok := ctx[0].Content.Parts[0].(message.TextPart); !ok || text.Text != "be terse" {
		t.Fatalf("system prompt = %+v", ctx[0].Content.Parts[0])
	}
}

func TestInferenceNode_RejectsBadTail(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{Model: ptr(inferencetest.DefaultFakeModel)})

	// Empty channel.
	if err := executeGraph(t, g, agent.NoopHost{}, agent.NewBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("empty channel error = %v, want validation-classified", err)
	}
	// Assistant tail — the loop must hand the turn to user or tool.
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		message.NewTextMessage(message.RoleAssistant, "hello"))
	if err := executeGraph(t, g, agent.NoopHost{}, board); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("assistant tail error = %v, want validation-classified", err)
	}
}

func TestInferenceNode_ToolPendingFlagAndCatalogTools(t *testing.T) {
	call := message.ToolCall{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"weather"}`)}
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	catalog := toolCatalog(t, tool.FuncTool(
		message.ToolDefinition{
			Name:        "search",
			Description: "search the web",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		func(_ context.Context, args string) (string, error) {
			return "sunny:" + args, nil
		},
	))
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t), Catalog: catalog})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:          ptr(inferencetest.DefaultFakeModel),
		Tools:          []string{"search"},
		ToolPendingKey: "tool_pending",
	})
	board := userBoard()
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if pending, _ := board.GetVar("tool_pending"); pending != true {
		t.Fatalf("tool_pending = %v, want true", pending)
	}
	msgs := board.Channel(agent.MainChannel)
	part, ok := msgs[1].Content.Parts[0].(message.ToolCallPart)
	if !ok || part.Call.ID != "call_1" || part.Call.Name != "search" {
		t.Fatalf("assistant part = %+v, want tool_call call_1/search", msgs[1].Content.Parts[0])
	}

	// The declared tool definition rode the text intent.
	intentTools := fake.LastRequest().Input.Content.Intent.Text.Tools
	if len(intentTools) != 1 || intentTools[0].Name != "search" {
		t.Fatalf("intent tools = %+v, want search", intentTools)
	}
}

func TestInferenceNode_UnknownToolRejected(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t), Catalog: toolCatalog(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Tools: []string{"ghost"},
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unknown tool error = %v, want validation-classified", err)
	}
}

func TestInferenceNode_UndefinedToolRecovery(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.ToolCallPart{Call: message.ToolCall{
							ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`),
						}},
					}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog:  toolCatalog(t, stubTool{name: "known", desc: "a known tool"}),
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:                 ptr(inferencetest.DefaultFakeModel),
		Tools:                 []string{"known"},
		ToolPendingKey:        "tool_pending",
		UndefinedToolRecovery: &UndefinedToolRecoveryConfig{Enabled: true, MaxPerRun: 2},
		RecoverPendingKey:     "recover_pending",
		RecoverCountKey:       "recover_count",
	})
	board := userBoard()
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 3 {
		t.Fatalf("channel length = %d, want 3 (user + assistant call + tool rejection)", len(msgs))
	}
	call, ok := msgs[1].Content.Parts[0].(message.ToolCallPart)
	if !ok || msgs[1].Role != message.RoleAssistant || call.Call.Name != "ghost" {
		t.Fatalf("recovered assistant message = %+v", msgs[1])
	}
	result, ok := msgs[2].Content.Parts[0].(message.ToolResultPart)
	if !ok || msgs[2].Role != message.RoleTool || !result.Result.IsError ||
		result.Result.CallID != "call-1" || !strings.Contains(result.Result.Content, "tool_search") {
		t.Fatalf("recovered tool message = %+v", msgs[2])
	}
	if v, _ := board.GetVar("recover_pending"); v != true {
		t.Fatalf("recover_pending = %v, want true", v)
	}
	if v, _ := board.GetVar("recover_count"); v != 1 {
		t.Fatalf("recover_count = %v, want 1", v)
	}
	if v, _ := board.GetVar("tool_pending"); v != false {
		t.Fatalf("tool_pending = %v, want false", v)
	}
}

func TestInferenceNode_UndefinedToolRecoveryDisabled(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.ToolCallPart{Call: message.ToolCall{
							ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`),
						}},
					}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog:  toolCatalog(t, stubTool{name: "known", desc: "a known tool"}),
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Tools: []string{"known"},
	})
	board := userBoard()
	err := executeGraph(t, g, agent.NoopHost{}, board)
	var infErr *inference.Error
	if !errors.As(err, &infErr) || infErr.Kind != inference.UndefinedTool {
		t.Fatalf("error = %v, want undefined_tool", err)
	}
	if msgs := board.Channel(agent.MainChannel); len(msgs) != 1 {
		t.Fatalf("channel length = %d, want 1 (no feedback when recovery disabled)", len(msgs))
	}
}

func TestInferenceNode_UndefinedToolRecoveryBudget(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.ToolCallPart{Call: message.ToolCall{
							ID: "call-1", Name: "ghost", Arguments: json.RawMessage(`{}`),
						}},
					}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog:  toolCatalog(t, stubTool{name: "known", desc: "a known tool"}),
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:                 ptr(inferencetest.DefaultFakeModel),
		Tools:                 []string{"known"},
		UndefinedToolRecovery: &UndefinedToolRecoveryConfig{Enabled: true, MaxPerRun: 1},
		RecoverPendingKey:     "recover_pending",
		RecoverCountKey:       "recover_count",
	})
	board := userBoard()
	board.SetVar("recover_count", 1) // one recovery already spent this run
	err := executeGraph(t, g, agent.NoopHost{}, board)
	var infErr *inference.Error
	if !errors.As(err, &infErr) || infErr.Kind != inference.UndefinedTool {
		t.Fatalf("error = %v, want undefined_tool past budget", err)
	}
	if msgs := board.Channel(agent.MainChannel); len(msgs) != 1 {
		t.Fatalf("channel length = %d, want 1 (no feedback past budget)", len(msgs))
	}
}

func TestInferenceNode_UndefinedToolRecoveryRequiresKeys(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:                 ptr(inferencetest.DefaultFakeModel),
		UndefinedToolRecovery: &UndefinedToolRecoveryConfig{Enabled: true},
	})
	err := executeGraph(t, g, agent.NoopHost{}, userBoard())
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("missing keys error = %v, want validation", err)
	}
}

func TestInferenceNode_UndefinedToolRecoveryClearsMarker(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:             ptr(inferencetest.DefaultFakeModel),
		RecoverPendingKey: "recover_pending",
	})
	board := userBoard()
	board.SetVar("recover_pending", true)
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if v, _ := board.GetVar("recover_pending"); v != false {
		t.Fatalf("recover_pending = %v, want false after success", v)
	}
}

func TestInferenceNode_UndefinedToolRecoveryKeepsNamedChoiceStrict(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.ToolCallPart{Call: message.ToolCall{
							ID: "call-1", Name: "other", Arguments: json.RawMessage(`{}`),
						}},
					}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog: toolCatalog(t,
			stubTool{name: "known", desc: "the named-choice tool"},
			stubTool{name: "other", desc: "a defined but unchosen tool"},
		),
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:                 ptr(inferencetest.DefaultFakeModel),
		Tools:                 []string{"known", "other"},
		ToolChoice:            &inference.ToolChoice{Kind: inference.ToolChoiceNamed, Name: "known"},
		UndefinedToolRecovery: &UndefinedToolRecoveryConfig{Enabled: true, MaxPerRun: 2},
		RecoverPendingKey:     "recover_pending",
		RecoverCountKey:       "recover_count",
	})
	board := userBoard()
	err := executeGraph(t, g, agent.NoopHost{}, board)
	var infErr *inference.Error
	if !errors.As(err, &infErr) || infErr.Kind != inference.InvalidProviderResponse {
		t.Fatalf("error = %v, want invalid_provider_response for named-choice violation", err)
	}
	if msgs := board.Channel(agent.MainChannel); len(msgs) != 1 {
		t.Fatalf("channel length = %d, want 1 (named choice never recovered)", len(msgs))
	}
}

// fakeVisibleCatalog is a minimal stand-in for a catalog whose
// Definitions are the final model-visible set (the dynamic injection
// view being one such implementation): it accepts RequiredByName
// declarations through an optional interface the node does not depend
// on.
type fakeVisibleCatalog struct {
	defs     []message.ToolDefinition
	required []string
	advances int
}

func (f *fakeVisibleCatalog) Get(string) (tool.Tool, bool) { return nil, false }
func (f *fakeVisibleCatalog) Definitions() []message.ToolDefinition {
	return f.defs
}
func (f *fakeVisibleCatalog) Require(names ...string) {
	f.required = append(f.required, names...)
}
func (f *fakeVisibleCatalog) AdvanceTurn()                { f.advances++ }
func (f *fakeVisibleCatalog) Select(...string)            {}
func (f *fakeVisibleCatalog) RecordCall(message.ToolCall) {}
func (f *fakeVisibleCatalog) Load(context.Context) error  { return nil }
func (f *fakeVisibleCatalog) EnsureLoaded(context.Context, ...string) error {
	return nil
}
func (f *fakeVisibleCatalog) Search(context.Context, string, int) ([]tool.SearchHit, error) {
	return nil, errdefs.NotAvailablef("test catalog has no search")
}
func (f *fakeVisibleCatalog) SearchWithLoad(context.Context, string, int) ([]tool.SearchHit, error) {
	return nil, errdefs.NotAvailablef("test catalog has no search")
}

func TestInferenceNode_AllTools(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	catalog := &fakeVisibleCatalog{
		defs: []message.ToolDefinition{
			{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "archive", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t), Catalog: catalog})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:    ptr(inferencetest.DefaultFakeModel),
		Tools:    []string{"search"},
		AllTools: true,
	})
	board := userBoard()
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(catalog.required) != 1 || catalog.required[0] != "search" {
		t.Errorf("RequiredByName = %v, want [search]", catalog.required)
	}
	intentTools := fake.LastRequest().Input.Content.Intent.Text.Tools
	if len(intentTools) != 2 {
		t.Fatalf("intent tools = %+v, want catalog-defined set", intentTools)
	}
	if intentTools[0].Name != "search" || intentTools[1].Name != "archive" {
		t.Errorf("intent tool names = %v, want [search archive]", intentTools)
	}
	if catalog.advances != 1 {
		t.Errorf("AdvanceTurn calls = %d, want 1 per round", catalog.advances)
	}
}

func TestInferenceNode_AllToolsUsesContextOverride(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	override := &fakeVisibleCatalog{
		defs: []message.ToolDefinition{
			{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
			{Name: "archive", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	// The bound catalog is a plain empty registry; the override on the
	// execution context must win in all_tools mode.
	bound := toolCatalog(t)
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog:  bound,
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:    ptr(inferencetest.DefaultFakeModel),
		Tools:    []string{"search"},
		AllTools: true,
	})

	ctx := tool.WithSession(context.Background(), override)
	board := userBoard()
	if _, err := g.Execute(ctx,
		agent.Run{Identity: agent.Identity{AgentID: "test-agent", RunID: "run-1"}},
		agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	intentTools := fake.LastRequest().Input.Content.Intent.Text.Tools
	if len(intentTools) != 2 {
		t.Fatalf("intent tools = %+v, want the context override's set", intentTools)
	}
	if len(override.required) != 1 || override.required[0] != "search" {
		t.Errorf("RequiredByName = %v, want [search]", override.required)
	}
	if override.advances != 1 {
		t.Errorf("AdvanceTurn calls = %d, want 1", override.advances)
	}
}

func TestInferenceNode_AllToolsRejectsUnknownName(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	catalog := &fakeVisibleCatalog{
		defs: []message.ToolDefinition{
			{Name: "search", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t), Catalog: catalog})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:    ptr(inferencetest.DefaultFakeModel),
		Tools:    []string{"ghost"},
		AllTools: true,
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unknown tool in dynamic mode = %v, want Validation", err)
	}
}

func TestInferenceNode_AllToolsRequiresCatalog(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:    ptr(inferencetest.DefaultFakeModel),
		AllTools: true,
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err == nil {
		t.Fatal("AllTools without a catalog succeeded, want error")
	}
}

func TestInferenceNode_RouterPathWhenNoModel(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	runtime := fake.Assembly(t)
	reg := inferenceRegistry(t, InferenceNodeDeps{Router: fakeRouter(t, runtime)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if req := fake.LastRequest(); req.Input.Role != inference.InputRoleUser {
		t.Fatalf("routed request input = %+v, want user", req.Input)
	}
}

func TestInferenceNode_NotAvailable(t *testing.T) {
	reg := inferenceRegistry(t, InferenceNodeDeps{})

	// Model configured, no runtime.
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{Model: ptr(inferencetest.DefaultFakeModel)})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("model-without-runtime error = %v, want NotAvailable", err)
	}
	// No model, no router.
	g = singleNodeGraph(t, reg, "inference", InferenceConfig{})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err == nil || !errdefs.IsNotAvailable(err) {
		t.Fatalf("no-model-no-router error = %v, want NotAvailable", err)
	}
}

func TestInferenceNode_Stream_PublishesDeltasAppendsAssembled(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "hel"}},
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "lo"}},
			{FinishReason: inference.FinishCompleted},
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Stream: true,
	})
	host := &captureHost{}
	board := userBoard()
	if err := executeGraph(t, g, host, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Subscribers saw one token event per text delta, in order.
	deltas := host.published(agent.SubjectStreamDelta("run-1", "test-agent.node.n"))
	if len(deltas) != 3 {
		t.Fatalf("stream-delta envelopes = %d, want 3", len(deltas))
	}
	for i, want := range []string{"hel", "lo"} {
		var payload agent.StreamDeltaPayload
		if err := deltas[i].Decode(&payload); err != nil {
			t.Fatalf("delta %d payload: %v", i, err)
		}
		text, ok := payload.Part.(message.TextPart)
		if payload.Type != agent.StreamDeltaPart || !ok || text.Text != want {
			t.Fatalf("delta %d = %+v, want text part %q", i, payload, want)
		}
	}
	var finish agent.StreamDeltaPayload
	if err := deltas[2].Decode(&finish); err != nil {
		t.Fatalf("finish delta payload: %v", err)
	}
	if finish.Type != agent.StreamDeltaFinish ||
		finish.FinishReason != string(inference.FinishCompleted) {
		t.Fatalf("finish delta = %+v, want completed finish", finish)
	}

	// …and the board still received exactly one assembled message.
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 {
		t.Fatalf("channel len = %d, want user + one assistant", len(msgs))
	}
	if text, ok := msgs[1].Content.Parts[0].(message.TextPart); !ok || text.Text != "hello" {
		t.Fatalf("assembled message = %+v, want text %q", msgs[1].Content.Parts[0], "hello")
	}
}

func TestInferenceNode_Stream_PublishesReasoningDeltas(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.ReasoningDelta{Text: "thin"}},
			{PartIndex: 0, Delta: inference.ReasoningDelta{Text: "king"}},
			{PartIndex: 1, Delta: inference.TextPartDelta{Text: "answer"}},
			{PartIndex: 0, Delta: inference.ReasoningDelta{Signature: "sig-1", ID: "trace-1"}},
			{FinishReason: inference.FinishCompleted},
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Stream: true,
	})
	host := &captureHost{}
	board := userBoard()
	if err := executeGraph(t, g, host, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// Subscribers saw each reasoning fragment as an incremental
	// reasoning part, interleaved with the text token, and no
	// duplicate complete-part emission at the end.
	deltas := host.published(agent.SubjectStreamDelta("run-1", "test-agent.node.n"))
	want := []message.Part{
		message.ReasoningPart{Text: "thin"},
		message.ReasoningPart{Text: "king"},
		message.TextPart{Text: "answer"},
		message.ReasoningPart{Signature: "sig-1", ID: "trace-1"},
	}
	if len(deltas) != len(want)+1 {
		t.Fatalf("stream-delta envelopes = %d, want %d", len(deltas), len(want)+1)
	}
	for i, part := range want {
		var payload agent.StreamDeltaPayload
		if err := deltas[i].Decode(&payload); err != nil {
			t.Fatalf("delta %d payload: %v", i, err)
		}
		if payload.Type != agent.StreamDeltaPart {
			t.Fatalf("delta %d type = %q, want part", i, payload.Type)
		}
		if !reflect.DeepEqual(payload.Part, part) {
			t.Fatalf("delta %d part = %#v, want %#v", i, payload.Part, part)
		}
	}
	var finish agent.StreamDeltaPayload
	if err := deltas[len(want)].Decode(&finish); err != nil {
		t.Fatalf("finish delta payload: %v", err)
	}
	if finish.Type != agent.StreamDeltaFinish ||
		finish.FinishReason != string(inference.FinishCompleted) {
		t.Fatalf("finish delta = %+v, want completed finish", finish)
	}

	// …and the board still received exactly one assembled message
	// with the complete reasoning trace plus the answer text.
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 {
		t.Fatalf("channel len = %d, want user + one assistant", len(msgs))
	}
	parts := msgs[1].Content.Parts
	if len(parts) != 2 {
		t.Fatalf("assembled parts = %d, want 2", len(parts))
	}
	reasoning, ok := parts[0].(message.ReasoningPart)
	if !ok || reasoning.Text != "thinking" ||
		reasoning.Signature != "sig-1" || reasoning.ID != "trace-1" {
		t.Fatalf("assembled reasoning = %#v", parts[0])
	}
	if text, ok := parts[1].(message.TextPart); !ok || text.Text != "answer" {
		t.Fatalf("assembled text = %#v", parts[1])
	}
}

func TestInferenceNode_EmitsProviderOutputsAndFinishMetadata(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "hi"}},
			{
				FinishReason:    inference.FinishCompleted,
				ProviderOutputs: inference.ProviderOutputs{testProviderOutput{Query: "flowcraft"}},
				RequestID:       "req-1",
				ResponseID:      "resp-1",
			},
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Stream: true,
	})
	host := &captureHost{}
	if err := executeGraph(t, g, host, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	deltas := host.published(agent.SubjectStreamDelta("run-1", "test-agent.node.n"))
	if len(deltas) != 3 {
		t.Fatalf("stream-delta envelopes = %d, want 3", len(deltas))
	}

	var outputs agent.StreamDeltaPayload
	if err := deltas[1].Decode(&outputs); err != nil {
		t.Fatalf("provider_outputs payload: %v", err)
	}
	if outputs.Type != agent.StreamDeltaProviderOutputs || len(outputs.ProviderOutputs) != 1 {
		t.Fatalf("provider_outputs delta = %+v", outputs)
	}
	env := outputs.ProviderOutputs[0]
	if env.Provider != "fake" || env.Extension != "web_search" {
		t.Fatalf("provider_outputs envelope = %+v", env)
	}
	var output testProviderOutput
	if err := json.Unmarshal(env.Value, &output); err != nil {
		t.Fatalf("decode provider output value: %v", err)
	}
	if output.Query != "flowcraft" {
		t.Fatalf("provider output = %+v", output)
	}

	var finish agent.StreamDeltaPayload
	if err := deltas[2].Decode(&finish); err != nil {
		t.Fatalf("finish payload: %v", err)
	}
	if finish.Type != agent.StreamDeltaFinish ||
		finish.FinishReason != string(inference.FinishCompleted) ||
		finish.RequestID != "req-1" || finish.ResponseID != "resp-1" {
		t.Fatalf("finish delta = %+v", finish)
	}
}

func TestInferenceNode_StreamFailureReportsPartialUsage(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "hel"}},
			{Usage: &inference.Usage{InputTokens: 3, OutputTokens: 4, TotalTokens: 7}},
		},
		StreamErr:   errors.New("connection reset"),
		StreamErrAt: 2,
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Stream: true,
	})
	host := &captureHost{}
	if err := executeGraph(t, g, host, userBoard()); err == nil {
		t.Fatal("mid-stream failure must propagate")
	}
	host.mu.Lock()
	usages := append([]inference.Usage(nil), host.usages...)
	host.mu.Unlock()
	if len(usages) != 1 {
		t.Fatalf("usage reports = %d, want 1", len(usages))
	}
	if usages[0].TotalTokens != 7 ||
		usages[0].InputTokens != 3 || usages[0].OutputTokens != 4 {
		t.Fatalf("partial usage = %+v", usages[0])
	}
}

// testProviderOutput exercises the provider-outputs stream delta with
// a concrete, JSON-round-trippable output family.
type testProviderOutput struct {
	Query string `json:"query,omitempty"`
}

func (testProviderOutput) ProviderID() string  { return "fake" }
func (testProviderOutput) ExtensionID() string { return "web_search" }
func (testProviderOutput) Validate() error     { return nil }
func (o testProviderOutput) Clone() inference.ProviderOutput {
	return o
}

// testExtension mirrors a provider option struct for the extensions
// wire path.
type testExtension struct {
	CacheKey string `json:"cache_key,omitempty"`
}

func (e testExtension) ProviderID() string  { return "fake" }
func (e testExtension) ExtensionID() string { return "generate_options" }
func (e testExtension) ActiveFields() []inference.ExtensionField {
	if e.CacheKey == "" {
		return nil
	}
	return []inference.ExtensionField{"cache_key"}
}
func (e testExtension) Validate() error            { return nil }
func (e testExtension) Clone() inference.Extension { return e }

func TestInferenceNode_Extensions(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Extensions: map[string]inference.ExtensionDecoder{
			"fake/generate_options": inference.ExtensionDecoderFor(func() *testExtension { return &testExtension{} }),
		},
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Extensions: []inference.ExtensionEntry{{
			Provider: "fake",
			ID:       "generate_options",
			Fields:   json.RawMessage(`{"cache_key":"sess-1"}`),
		}},
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	exts := fake.LastRequest().Extensions
	if len(exts) != 1 {
		t.Fatalf("extensions = %+v, want one", exts)
	}
	if ext, ok := exts[0].(testExtension); !ok || ext.CacheKey != "sess-1" {
		t.Fatalf("extension = %+v (%T), want cache_key sess-1", exts[0], exts[0])
	}
}

func TestInferenceNode_ResponseFormat_ReachesRequest(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: `{"answer":"42"}`},
					}},
				},
				FinishReason: inference.FinishCompleted,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Intent: &inference.Intent{Text: &inference.TextIntent{
			Response: &inference.ResponseFormat{
				Kind: inference.ResponseJSONSchema,
				Name: "answer",
				Schema: json.RawMessage(
					`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`,
				),
			},
		},
		},
	})
	board := userBoard()
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	req := fake.LastRequest()
	response := req.Input.Content.Intent.Text.Response
	if response == nil || response.Kind != inference.ResponseJSONSchema ||
		response.Name != "answer" || len(response.Schema) == 0 {
		t.Fatalf("request response format = %#v, want json_schema answer", response)
	}
	msgs := board.Channel(agent.MainChannel)
	if text, ok := msgs[len(msgs)-1].Content.Parts[0].(message.TextPart); !ok ||
		text.Text != `{"answer":"42"}` {
		t.Fatalf("assistant message = %+v, want structured JSON", msgs[len(msgs)-1].Content.Parts[0])
	}
}

func TestInferenceNode_ResponseFormat_Enforced(t *testing.T) {
	cases := []struct {
		name    string
		format  *inference.ResponseFormat
		respond string
		want    string
	}{
		{
			name:    "json_object rejects array",
			respond: `["a","b"]`,
			format: &inference.ResponseFormat{
				Kind: inference.ResponseJSONObject,
			},
			want: "structured generate response must be a JSON object",
		},
		{
			name:    "json_object rejects plain text",
			respond: "ok",
			format: &inference.ResponseFormat{
				Kind: inference.ResponseJSONObject,
			},
			want: "structured generate response is not valid JSON",
		},
		{
			name:    "json_schema rejects non-conforming JSON",
			respond: `{"answer":42}`,
			format: &inference.ResponseFormat{
				Kind: inference.ResponseJSONSchema,
				Name: "answer",
				Schema: json.RawMessage(
					`{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"]}`,
				),
			},
			want: "does not match requested JSON schema",
		},
		{
			name:    "json_schema array root rejects object",
			respond: `{"a":"b"}`,
			format: &inference.ResponseFormat{
				Kind: inference.ResponseJSONSchema,
				Name: "answer",
				Schema: json.RawMessage(
					`{"type":"array","items":{"type":"string"}}`,
				),
			},
			want: "does not match requested JSON schema",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &inferencetest.GenerateFake{
				Respond: func(inference.GenerateRequest) inference.GenerateResponse {
					return inference.GenerateResponse{
						Message: message.Message{
							Role: message.RoleAssistant,
							Content: message.Content{Parts: []message.Part{
								message.TextPart{Text: tc.respond},
							}},
						},
						FinishReason: inference.FinishCompleted,
					}
				},
			}
			reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
			g := singleNodeGraph(t, reg, "inference", InferenceConfig{
				Model: ptr(inferencetest.DefaultFakeModel),
				Intent: &inference.Intent{
					Text: &inference.TextIntent{Response: tc.format},
				},
			})
			err := executeGraph(t, g, agent.NoopHost{}, userBoard())
			if err == nil || !strings.Contains(responseCause(err), tc.want) {
				t.Fatalf("Execute error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestInferenceNode_ResponseFormat_ArraySchema(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.TextPart{Text: `["a","b"]`},
					}},
				},
				FinishReason: inference.FinishCompleted,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Intent: &inference.Intent{Text: &inference.TextIntent{
			Response: &inference.ResponseFormat{
				Kind: inference.ResponseJSONSchema,
				Name: "answer",
				Schema: json.RawMessage(
					`{"type":"array","items":{"type":"string"}}`,
				),
			},
		},
		},
	})
	board := userBoard()
	if err := executeGraph(t, g, agent.NoopHost{}, board); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	msgs := board.Channel(agent.MainChannel)
	if text, ok := msgs[len(msgs)-1].Content.Parts[0].(message.TextPart); !ok ||
		text.Text != `["a","b"]` {
		t.Fatalf("assistant message = %+v, want array JSON", msgs[len(msgs)-1].Content.Parts[0])
	}
}

// responseCause collects every layer of the error chain so assertions
// can see the provider-response detail behind the kind/operation
// envelope and its errdefs classification wrapper.
func responseCause(err error) string {
	var parts []string
	for e := err; e != nil; {
		parts = append(parts, e.Error())
		var inferErr *inference.Error
		if errors.As(e, &inferErr) && inferErr.Unwrap() != nil {
			e = inferErr.Unwrap()
			continue
		}
		e = errors.Unwrap(e)
	}
	return strings.Join(parts, " | ")
}

func TestInferenceNode_ResponseFormat_RejectsInvalidSchema(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Intent: &inference.Intent{Text: &inference.TextIntent{
			Response: &inference.ResponseFormat{
				Kind:   inference.ResponseJSONSchema,
				Name:   "answer",
				Schema: json.RawMessage(`{"type":1}`),
			},
		},
		},
	})
	err := executeGraph(t, g, agent.NoopHost{}, userBoard())
	if err == nil || !strings.Contains(responseCause(err), "schema") {
		t.Fatalf("Execute error = %v, want schema validation failure", err)
	}
}

func TestInferenceNode_Intent_ImageReachesRequest(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						imagePart(t, "https://x/1.png"),
						imagePart(t, "https://x/2.png"),
					}},
				},
				FinishReason: inference.FinishCompleted,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Intent: &inference.Intent{
			Image: &inference.ImageIntent{Count: ptr(2)},
		},
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	intent := fake.LastRequest().Input.Content.Intent
	if intent.Image == nil || intent.Image.Count == nil || *intent.Image.Count != 2 {
		t.Fatalf("image intent = %+v, want count 2", intent.Image)
	}
	if intent.Text != nil {
		t.Fatalf("text intent = %+v, want nil for an image-only request", intent.Text)
	}
}

func imagePart(t *testing.T, rawURL string) message.ImagePart {
	t.Helper()
	src, err := media.NewImageURL(rawURL, "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.ImagePart{Source: src}
}

func TestInferenceNode_Intent_TextControlsReachRequest(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Intent: &inference.Intent{Text: &inference.TextIntent{
			Temperature:      ptr(0.7),
			TopP:             ptr(0.9),
			MaxOutputTokens:  ptr(512),
			ReasoningEnabled: ptr(true),
			ReasoningEffort:  inference.ReasoningHigh,
		}},
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	text := fake.LastRequest().Input.Content.Intent.Text
	if text.Temperature == nil || *text.Temperature != 0.7 {
		t.Fatalf("temperature = %+v, want 0.7", text.Temperature)
	}
	if text.TopP == nil || *text.TopP != 0.9 {
		t.Fatalf("top_p = %+v, want 0.9", text.TopP)
	}
	if text.MaxOutputTokens == nil || *text.MaxOutputTokens != 512 {
		t.Fatalf("max_output_tokens = %+v, want 512", text.MaxOutputTokens)
	}
	if text.ReasoningEnabled == nil || !*text.ReasoningEnabled {
		t.Fatalf("reasoning_enabled = %+v, want true", text.ReasoningEnabled)
	}
	if text.ReasoningEffort != inference.ReasoningHigh {
		t.Fatalf("reasoning_effort = %q, want high", text.ReasoningEffort)
	}
}

func TestInferenceNode_Intent_ToolsMergeIntoTextIntent(t *testing.T) {
	catalog := toolCatalog(t, tool.FuncTool(
		message.ToolDefinition{
			Name:        "search",
			Description: "search the web",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		func(_ context.Context, args string) (string, error) {
			return "sunny:" + args, nil
		},
	))
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role: message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{
						message.ToolCallPart{Call: message.ToolCall{
							ID:        "call_1",
							Name:      "search",
							Arguments: json.RawMessage(`{"q":"weather"}`),
						}},
					}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t), Catalog: catalog})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Tools: []string{"search"},
		ToolChoice: &inference.ToolChoice{
			Kind: inference.ToolChoiceNamed,
			Name: "search",
		},
		Intent: &inference.Intent{Text: &inference.TextIntent{
			Temperature: ptr(0.7),
		}},
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	text := fake.LastRequest().Input.Content.Intent.Text
	if text.Temperature == nil || *text.Temperature != 0.7 {
		t.Fatalf("temperature = %+v, want 0.7", text.Temperature)
	}
	if len(text.Tools) != 1 || text.Tools[0].Name != "search" {
		t.Fatalf("intent tools = %+v, want search", text.Tools)
	}
	if text.ToolChoice == nil || text.ToolChoice.Kind != inference.ToolChoiceNamed ||
		text.ToolChoice.Name != "search" {
		t.Fatalf("tool_choice = %+v, want named search", text.ToolChoice)
	}
}

func TestInferenceNode_Intent_NonTextWithToolsRejected(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog:  toolCatalog(t),
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Tools:  []string{"search"},
		Intent: &inference.Intent{Image: &inference.ImageIntent{}},
	})
	err := executeGraph(t, g, agent.NoopHost{}, userBoard())
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "no text modality") {
		t.Fatalf("Execute error = %v, want validation about missing text modality", err)
	}
}

func TestInferenceNode_Intent_DoubleToolDeclarationRejected(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{
		Assembly: fake.Assembly(t),
		Catalog:  toolCatalog(t),
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Tools: []string{"search"},
		Intent: &inference.Intent{Text: &inference.TextIntent{
			Tools: []message.ToolDefinition{{Name: "search"}},
		}},
	})
	err := executeGraph(t, g, agent.NoopHost{}, userBoard())
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "declared both") {
		t.Fatalf("Execute error = %v, want validation about double tool declaration", err)
	}
}

func TestInferenceNode_Intent_EmptyRejected(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Intent: &inference.Intent{},
	})
	err := executeGraph(t, g, agent.NoopHost{}, userBoard())
	if err == nil || !errdefs.IsValidation(err) ||
		!strings.Contains(err.Error(), "requires text, image, audio, or video") {
		t.Fatalf("Execute error = %v, want validation about empty intent", err)
	}
}

func TestInferenceNode_RejectsLegacyTextKnobs(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	_, err := graph.Build(&graph.GraphDefinition{
		Name:  "test-graph",
		Entry: "n",
		Nodes: []graph.NodeDefinition{{
			ID:   "n",
			Type: "inference",
			Config: mustConfig(t, map[string]any{
				"model": map[string]any{
					"id": map[string]any{"provider": "fake", "name": "fake-1"},
				},
				"temperature": 0.7,
			}),
		}},
	}, reg)
	if err == nil || !strings.Contains(err.Error(), "unknown config field") ||
		!strings.Contains(err.Error(), "temperature") {
		t.Fatalf("Build error = %v, want unknown-field rejection for temperature", err)
	}
}

func ptr[T any](v T) *T { return &v }

// TestInferenceNode_StreamMidFailureCommitsPartial proves a stream
// that dies mid-generation still lands its buffered text on the board
// as one partial assistant message before the error propagates — the
// progress survives the failed node instead of vanishing with it.
func TestInferenceNode_StreamMidFailureCommitsPartial(t *testing.T) {
	fake := &inferencetest.GenerateFake{
		Events: []inference.GenerateStreamEvent{
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "hel"}},
			{PartIndex: 0, Delta: inference.TextPartDelta{Text: "lo"}},
			{FinishReason: inference.FinishCompleted},
		},
		StreamErr:   errors.New("connection reset"),
		StreamErrAt: 2, // both deltas delivered, then the stream dies
	}
	reg := inferenceRegistry(t, InferenceNodeDeps{Assembly: fake.Assembly(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:  ptr(inferencetest.DefaultFakeModel),
		Stream: true,
	})
	board := userBoard()
	if err := executeGraph(t, g, &captureHost{}, board); err == nil {
		t.Fatal("mid-stream failure must propagate")
	}

	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 {
		t.Fatalf("channel len = %d, want user + partial assistant", len(msgs))
	}
	if msgs[1].Role != message.RoleAssistant || msgs[1].Content.Text() != "hello" {
		t.Fatalf("partial message = %+v, want assistant \"hello\"", msgs[1])
	}
}

type resultFailingStream struct {
	next int
	err  error
}

func (s *resultFailingStream) Next(context.Context) (inference.GenerateStreamEvent, error) {
	if s.next > 0 {
		return inference.GenerateStreamEvent{}, io.EOF
	}
	s.next++
	return inference.GenerateStreamEvent{
		PartIndex: 0,
		Delta:     inference.TextPartDelta{Text: "partial"},
	}, nil
}

func (s *resultFailingStream) Result() (inference.GenerateResponse, error) {
	return inference.GenerateResponse{}, s.err
}

func (*resultFailingStream) Close() error { return nil }

func TestInferenceNode_StreamResultFailureCommitsPartialExactlyOnce(t *testing.T) {
	board := userBoard()
	stream := &resultFailingStream{err: errors.New("invalid terminal response")}
	ec := graph.ExecutionContext{Context: context.Background(), Host: agent.NoopHost{}, NodeID: "n"}

	if _, err := drainGenerateStream(ec, board, "", stream); !errors.Is(err, stream.err) {
		t.Fatalf("drainGenerateStream error = %v, want %v", err, stream.err)
	}
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 {
		t.Fatalf("channel len = %d, want user + exactly one partial assistant", len(msgs))
	}
	if msgs[1].Role != message.RoleAssistant || msgs[1].Content.Text() != "partial" {
		t.Fatalf("partial message = %+v", msgs[1])
	}
}

func TestInferenceNode_SuccessRecordsRequestIDOnNodeSpan(t *testing.T) {
	prev := otel.GetTracerProvider()
	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		_ = tp.Shutdown(context.Background())
		otel.SetTracerProvider(prev)
	})

	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: message.Message{
					Role:    message.RoleAssistant,
					Content: message.Content{Parts: []message.Part{message.TextPart{Text: "ok"}}},
				},
				FinishReason: inference.FinishCompleted,
				Metadata: inference.Metadata{
					RequestID:  "req-node-ok",
					ResponseID: "resp-node-ok",
				},
				Usage: inference.Usage{
					InputTokens:  10,
					OutputTokens: 3,
					TotalTokens:  13,
					Model:        inferencetest.DefaultFakeModel,
				},
			}
		},
	}
	// Routed path: the assembly-level telemetry records onto the route
	// span, so request/response ids reaching the node span must come
	// from the node's own RecordLLMIDs call.
	runtime := fake.Assembly(t)
	reg := inferenceRegistry(t, InferenceNodeDeps{Router: fakeRouter(t, runtime)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var nodeSpan sdktrace.ReadOnlySpan
	for _, span := range rec.Ended() {
		if span.Name() == "node.inference.execute" {
			nodeSpan = span
			break
		}
	}
	if nodeSpan == nil {
		t.Fatal("node span not found")
	}
	var requestID, responseID string
	var haveRequest, haveResponse bool
	for _, kv := range nodeSpan.Attributes() {
		switch kv.Key {
		case telemetry.AttrLLMRequestID:
			requestID, haveRequest = kv.Value.AsString(), true
		case telemetry.AttrLLMResponseID:
			responseID, haveResponse = kv.Value.AsString(), true
		}
	}
	if !haveRequest || requestID != "req-node-ok" {
		t.Fatalf("node span llm.request.id = %q/%v, want req-node-ok", requestID, haveRequest)
	}
	if !haveResponse || responseID != "resp-node-ok" {
		t.Fatalf("node span llm.response.id = %q/%v, want resp-node-ok", responseID, haveResponse)
	}
	var inputTokens, outputTokens, totalTokens int64
	var haveInput, haveOutput, haveTotal bool
	for _, kv := range nodeSpan.Attributes() {
		switch kv.Key {
		case telemetry.AttrLLMInputTokens:
			inputTokens, haveInput = kv.Value.AsInt64(), true
		case telemetry.AttrLLMOutputTokens:
			outputTokens, haveOutput = kv.Value.AsInt64(), true
		case telemetry.AttrLLMTotalTokens:
			totalTokens, haveTotal = kv.Value.AsInt64(), true
		}
	}
	if !haveInput || inputTokens != 10 {
		t.Fatalf("node span llm.tokens.input = %d/%v, want 10", inputTokens, haveInput)
	}
	if !haveOutput || outputTokens != 3 {
		t.Fatalf("node span llm.tokens.output = %d/%v, want 3", outputTokens, haveOutput)
	}
	if !haveTotal || totalTokens != 13 {
		t.Fatalf("node span llm.tokens.total = %d/%v, want 13", totalTokens, haveTotal)
	}
}
