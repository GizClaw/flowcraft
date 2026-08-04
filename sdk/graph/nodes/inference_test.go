package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/agent/bindings"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/graph"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/inferencetest"
	"github.com/GizClaw/flowcraft/sdk/inference/route"
	"github.com/GizClaw/flowcraft/sdk/tool"
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
}

func (h *captureHost) Publish(_ context.Context, env event.Envelope) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.envelopes = append(h.envelopes, env)
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
		inference.NewTextMessage(inference.RoleUser, "hi"))
	return board
}

func inferenceRegistry(t *testing.T, deps InferenceNodeDeps) *graph.Registry {
	t.Helper()
	reg := graph.NewRegistry()
	if err := graph.RegisterType(reg, "inference", Inference(deps)); err != nil {
		t.Fatalf("register inference: %v", err)
	}
	return reg
}

func fakeRouter(t *testing.T, runtime *inference.Runtime) *route.Router {
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
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t)})
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
	if len(msgs) != 2 || msgs[1].Role != inference.RoleAssistant {
		t.Fatalf("channel = %+v, want user + assistant", msgs)
	}
	if text, ok := msgs[1].Content.Parts[0].(inference.TextPart); !ok || text.Text != "ok" {
		t.Fatalf("assistant message = %+v, want text %q", msgs[1].Content.Parts[0], "ok")
	}
	if v, ok := board.GetVar("answer"); !ok {
		t.Fatal("output_key var missing")
	} else if msg, ok := v.(inference.Message); !ok || msg.Role != inference.RoleAssistant {
		t.Fatalf("output var = %T, want inference.Message", v)
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
	if text, ok := req.Input.Content.Parts[0].(inference.TextPart); !ok || text.Text != "hi" {
		t.Fatalf("input = %+v, want text %q", req.Input.Content.Parts[0], "hi")
	}
	if req.Input.Content.Intent.Text == nil {
		t.Fatal("text intent missing")
	}
}

func TestInferenceNode_SystemPromptPrepended(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model:        ptr(inferencetest.DefaultFakeModel),
		SystemPrompt: "be terse",
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	ctx := fake.LastRequest().Context
	if len(ctx) != 1 || ctx[0].Role != inference.RoleSystem {
		t.Fatalf("context = %+v, want one system message", ctx)
	}
	if text, ok := ctx[0].Content.Parts[0].(inference.TextPart); !ok || text.Text != "be terse" {
		t.Fatalf("system prompt = %+v", ctx[0].Content.Parts[0])
	}
}

func TestInferenceNode_RejectsBadTail(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t)})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{Model: ptr(inferencetest.DefaultFakeModel)})

	// Empty channel.
	if err := executeGraph(t, g, agent.NoopHost{}, agent.NewBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("empty channel error = %v, want validation-classified", err)
	}
	// Assistant tail — the loop must hand the turn to user or tool.
	board := agent.NewBoard()
	board.AppendChannelMessage(agent.MainChannel,
		inference.NewTextMessage(inference.RoleAssistant, "hello"))
	if err := executeGraph(t, g, agent.NoopHost{}, board); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("assistant tail error = %v, want validation-classified", err)
	}
}

func TestInferenceNode_ToolPendingFlagAndCatalogTools(t *testing.T) {
	call := tool.Call{ID: "call_1", Name: "search", Arguments: json.RawMessage(`{"q":"weather"}`)}
	fake := &inferencetest.GenerateFake{
		Respond: func(inference.GenerateRequest) inference.GenerateResponse {
			return inference.GenerateResponse{
				Message: inference.Message{
					Role:    inference.RoleAssistant,
					Content: inference.Content{Parts: []inference.Part{inference.ToolCallPart{Call: call}}},
				},
				FinishReason: inference.FinishToolCalls,
			}
		},
	}
	catalog := tool.NewRegistry()
	catalog.Register(tool.FuncTool(
		tool.Definition{
			Name:        "search",
			Description: "search the web",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		func(_ context.Context, args string) (string, error) {
			return "sunny:" + args, nil
		},
	))
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t), Catalog: catalog})
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
	part, ok := msgs[1].Content.Parts[0].(inference.ToolCallPart)
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
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t), Catalog: tool.NewRegistry()})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Tools: []string{"ghost"},
	})
	if err := executeGraph(t, g, agent.NoopHost{}, userBoard()); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("unknown tool error = %v, want validation-classified", err)
	}
}

func TestInferenceNode_RouterPathWhenNoModel(t *testing.T) {
	fake := &inferencetest.GenerateFake{}
	runtime := fake.Runtime(t)
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
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t)})
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
	deltas := host.published(agent.SubjectStreamDelta("run-1", "graph_node_n"))
	if len(deltas) != 2 {
		t.Fatalf("stream-delta envelopes = %d, want 2", len(deltas))
	}
	for i, want := range []string{"hel", "lo"} {
		var payload agent.StreamDeltaPayload
		if err := deltas[i].Decode(&payload); err != nil {
			t.Fatalf("delta %d payload: %v", i, err)
		}
		if payload.Type != agent.StreamDeltaToken || payload.Content != want {
			t.Fatalf("delta %d = %+v, want token %q", i, payload, want)
		}
	}

	// …and the board still received exactly one assembled message.
	msgs := board.Channel(agent.MainChannel)
	if len(msgs) != 2 {
		t.Fatalf("channel len = %d, want user + one assistant", len(msgs))
	}
	if text, ok := msgs[1].Content.Parts[0].(inference.TextPart); !ok || text.Text != "hello" {
		t.Fatalf("assembled message = %+v, want text %q", msgs[1].Content.Parts[0], "hello")
	}
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
		Runtime: fake.Runtime(t),
		Extensions: map[string]bindings.ExtensionDecoder{
			"fake/generate_options": bindings.ExtensionDecoderFor(func() *testExtension { return &testExtension{} }),
		},
	})
	g := singleNodeGraph(t, reg, "inference", InferenceConfig{
		Model: ptr(inferencetest.DefaultFakeModel),
		Extensions: []bindings.ExtensionEntry{{
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
	reg := inferenceRegistry(t, InferenceNodeDeps{Runtime: fake.Runtime(t)})
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
	if msgs[1].Role != inference.RoleAssistant || msgs[1].Content.Text() != "hello" {
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
	if msgs[1].Role != inference.RoleAssistant || msgs[1].Content.Text() != "partial" {
		t.Fatalf("partial message = %+v", msgs[1])
	}
}
