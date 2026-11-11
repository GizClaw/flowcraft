package nodes_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/nodes"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// stubProvider answers every request with one canned response.
type stubProvider struct {
	response inference.GenerateResponse
	requests []inference.GenerateRequest
}

func (p *stubProvider) Generate(_ context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	p.requests = append(p.requests, request)
	return p.response, nil
}

func (p *stubProvider) GenerateStream(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	response, err := p.Generate(ctx, request)
	if err != nil {
		return nil, err
	}
	var events []inference.StreamEvent
	if text := response.Message.Content.Text(); text != "" {
		events = append(events, inference.StreamEvent{Kind: inference.StreamText, Text: text})
	}
	return &stubStream{events: events, response: response}, nil
}

type stubStream struct {
	events   []inference.StreamEvent
	response inference.GenerateResponse
}

func (s *stubStream) Next(context.Context) (inference.StreamEvent, error) {
	if len(s.events) == 0 {
		return inference.StreamEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *stubStream) Result() (inference.GenerateResponse, error) { return s.response, nil }
func (*stubStream) Close() error                                  { return nil }

func testRuntime(t *testing.T, provider inference.Generator) *runtime.Runtime {
	t.Helper()
	root := t.TempDir()
	config := runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
		Tools: runtime.ToolsConfig{Enable: []string{"files"}},
	}
	rt, err := runtime.New(context.Background(), config, runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(json.RawMessage) (inference.Provider, error) {
			return inference.Provider{Generator: provider}, nil
		},
	}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// invokeFor builds the Invoke the engine would pass.
func invokeFor(snapshot map[string]graph.Value, config string) graph.Invoke {
	return graph.Invoke{
		Node:     "n",
		Config:   json.RawMessage(config),
		State:    func(key string) graph.Value { return snapshot[key] },
		Snapshot: func() graph.Snapshot { return graph.NewSnapshot(snapshot) },
	}
}

// drainNode pulls a node's stream to the end and returns its result and events.
func drainNode(t testing.TB, stream graph.NodeStream) (graph.Result, []agent.Event) {
	t.Helper()
	var events []agent.Event
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event != nil {
			events = append(events, event)
		}
	}
	result, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := stream.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return result, events
}

func updateFor(t *testing.T, result graph.Result, key string) graph.Value {
	t.Helper()
	for _, update := range result.State {
		if update.Key == key {
			return update.Value
		}
	}
	t.Fatalf("no update for %q in %+v", key, result.State)
	return graph.Value{}
}

func TestConditionWritesTheBranch(t *testing.T) {
	kind := nodes.Condition()
	snapshot := map[string]graph.Value{"count": graph.JSON([]byte("3"))}

	stream, err := kind.Invoke(context.Background(), invokeFor(snapshot,
		`{"key":"route","when":"state.count > 2","then":"long","else":"short"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)
	if got, _ := updateFor(t, result, "route").Text(); got != "long" {
		t.Errorf("route = %q, want long", got)
	}

	stream, err = kind.Invoke(context.Background(), invokeFor(snapshot,
		`{"key":"route","when":"state.count > 9","then":"long","else":"short"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ = drainNode(t, stream)
	if got, _ := updateFor(t, result, "route").Text(); got != "short" {
		t.Errorf("route = %q, want short", got)
	}
}

func TestModelAppendsTheAnswerAndTheCalls(t *testing.T) {
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": "/tmp/x"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	provider := &stubProvider{response: inference.GenerateResponse{
		Message: message.Message{
			Role: message.RoleAssistant,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "let me look"},
				message.ToolCallPart{Call: call},
			}},
		},
	}}
	rt := testRuntime(t, provider)
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "read x"),
		})),
	}

	modelStream, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","pending":"pending"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, events := drainNode(t, modelStream)
	var streamed string
	for _, event := range events {
		if text, ok := event.(agent.Text); ok {
			streamed += text.Text
		}
	}
	if streamed != "let me look" {
		t.Errorf("streamed text = %q, want the delta", streamed)
	}
	transcript, err := updateFor(t, result, "messages").Messages()
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	// The node returns this step's write only; the engine appends it.
	if transcript.Len() != 1 {
		t.Fatalf("delta = %d messages, want 1", transcript.Len())
	}
	if role := transcript.Slice()[0].Role; role != message.RoleAssistant {
		t.Errorf("appended role = %q, want assistant", role)
	}
	pending, err := updateFor(t, result, "pending").JSON()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	var calls []message.ToolCall
	if err := json.Unmarshal(pending, &calls); err != nil {
		t.Fatalf("pending decode: %v", err)
	}
	if len(calls) != 1 || calls[0].Name != "read_file" {
		t.Errorf("pending = %+v, want the read_file call", calls)
	}
	if len(provider.requests) != 1 || len(provider.requests[0].Context) != 0 {
		t.Errorf("request context = %+v, want the one input turn", provider.requests)
	}
}

func TestToolExecutesPendingCalls(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	root := rt.Files().Writable()[0]
	path := filepath.Join(root, "doc.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	pending, err := json.Marshal([]message.ToolCall{call})
	if err != nil {
		t.Fatalf("encode pending: %v", err)
	}
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "read x"),
		})),
		"pending": graph.JSON(pending),
	}

	toolStream, err := nodes.Tool(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","pending":"pending"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, toolStream)
	transcript, err := updateFor(t, result, "messages").Messages()
	if err != nil {
		t.Fatalf("transcript: %v", err)
	}
	// The node returns this step's write only; the engine appends it.
	if transcript.Len() != 1 {
		t.Fatalf("delta = %d messages, want 1", transcript.Len())
	}
	if role := transcript.Slice()[0].Role; role != message.RoleTool {
		t.Errorf("appended role = %q, want tool", role)
	}
	cleared, err := updateFor(t, result, "pending").JSON()
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if string(cleared) != "[]" {
		t.Errorf("pending after execution = %s, want []", cleared)
	}
}

// No pending calls is not an error: a cycle routes on the state, not on a failure.
func TestToolWithoutPendingIsANoOp(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice(nil)),
		"pending":  graph.JSON([]byte("[]")),
	}
	toolStream, err := nodes.Tool(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","pending":"pending"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, toolStream)
	if len(result.State) != 0 {
		t.Errorf("state updates = %+v, want none", result.State)
	}
}

// The request block maps onto the inference contract, and the write keys land.
func TestModelMapsTheRequestAndWrites(t *testing.T) {
	provider := &stubProvider{response: inference.GenerateResponse{
		Message: message.Message{
			Role:    message.RoleAssistant,
			Content: message.NewTextContent("the answer"),
		},
		Usage: usage.Usage{InputTokens: 3, OutputTokens: 4},
	}}
	rt := testRuntime(t, provider)
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleSystem, "existing system"),
			message.NewTextMessage(message.RoleUser, "read x"),
		})),
	}

	stream, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot, `{
		"messages": "messages",
		"usage": "usage",
		"output": "answer",
		"request": {
			"model": {"provider": "p", "name": "m", "profile": "luna"},
			"hint": "p/m",
			"system": "would be a second system",
			"tools": {"names": ["read_file"], "choice": {"mode": "required"}}
		}
	}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	result, _ := drainNode(t, stream)

	request := provider.requests[0]
	if request.Model.Profile != "luna" || request.Model.ID.Name != "m" {
		t.Errorf("model = %+v, want the configured profile", request.Model)
	}
	if request.ModelHint != "p/m" {
		t.Errorf("hint = %q", request.ModelHint)
	}
	// The context already opens with a system turn, so the configured one is
	// not prepended again.
	if len(request.Context) != 1 || request.Context[0].Role != message.RoleSystem {
		t.Fatalf("context = %+v, want the one existing system turn", request.Context)
	}
	if request.Tools.Len() != 1 || request.Tools.Definitions()[0].Name != "read_file" {
		t.Errorf("tools = %+v, want only read_file", request.Tools.Definitions())
	}
	if choice := request.Tools.Choice(); choice == nil || choice.Mode != inference.ToolChoiceRequired {
		t.Errorf("choice = %+v, want required", choice)
	}
	if got, _ := updateFor(t, result, "answer").Text(); got != "the answer" {
		t.Errorf("answer = %q", got)
	}
	usage, err := updateFor(t, result, "usage").JSON()
	if err != nil {
		t.Fatalf("usage: %v", err)
	}
	var accounting map[string]any
	if err := json.Unmarshal(usage, &accounting); err != nil {
		t.Fatalf("usage decode: %v", err)
	}
	if accounting == nil {
		t.Error("usage was not written")
	}
}

func TestModelRejectsUnknownToolName(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
	}
	if _, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","request":{"tools":{"names":["absent"]}}}`)); err == nil {
		t.Fatal("unknown tool name was accepted")
	}
}

func TestModelRejectsUnregisteredExtension(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
	}
	_, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","request":{"extensions":[{"provider":"p","id":"x"}]}}`))
	if err == nil || !strings.Contains(err.Error(), "not registered") {
		t.Fatalf("err = %v, want an unregistered-extension error", err)
	}
}

// dynamicRuntime wires the tools as deferred, so only a discovery (or an
// explicit pin) makes one visible.
func dynamicRuntime(t testing.TB, provider inference.Generator) *runtime.Runtime {
	t.Helper()
	root := t.TempDir()
	config := runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
		Tools: runtime.ToolsConfig{
			Enable:  []string{"files"},
			Dynamic: &runtime.DynamicConfig{},
		},
	}
	rt, err := runtime.New(context.Background(), config, runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(json.RawMessage) (inference.Provider, error) {
			return inference.Provider{Generator: provider}, nil
		},
	}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// The model offers what the run's catalog view exposes: a discovery made by
// tool_search earlier in the run is visible to the next call.
func TestModelUsesTheRunCatalogView(t *testing.T) {
	provider := &stubProvider{response: textResponse("ok")}
	rt := dynamicRuntime(t, provider)

	ctx := rt.NewRunContext(context.Background())
	view, ok := runtime.CatalogViewFrom(ctx)
	if !ok {
		t.Fatal("the run context carries no catalog view")
	}
	view.Discover("read_file")

	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "read a file"),
		})),
	}
	stream, err := nodes.Model(rt).Invoke(ctx, invokeFor(snapshot, `{"messages":"messages"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	drainNode(t, stream)
	if len(provider.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(provider.requests))
	}
	names := definitionNames(provider.requests[0].Tools.Definitions())
	if !contains(names, "read_file") {
		t.Fatalf("tools = %v, want read_file from the run view", names)
	}
}

// Naming a deferred tool pins it visible: an explicit choice must not depend on
// a discovery that may never have happened.
func TestNamesPinADeferredTool(t *testing.T) {
	provider := &stubProvider{response: textResponse("ok")}
	rt := dynamicRuntime(t, provider)
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
		})),
	}
	stream, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","request":{"tools":{"names":["read_file"]}}}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	drainNode(t, stream)
	names := definitionNames(provider.requests[0].Tools.Definitions())
	if len(names) != 1 || names[0] != "read_file" {
		t.Fatalf("tools = %v, want only the pinned read_file", names)
	}
}

func definitionNames(definitions []message.ToolDefinition) []string {
	names := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		names = append(names, definition.Name)
	}
	return names
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func textResponse(text string) inference.GenerateResponse {
	return inference.GenerateResponse{Message: message.NewTextMessage(message.RoleAssistant, text)}
}

// BenchmarkModelCall10k measures one model call over a pathological history: the
// per-call overhead must stay small and allocation-light regardless of length.
func BenchmarkModelCall10k(b *testing.B) {
	provider := &stubProvider{response: textResponse("ok")}
	rt := dynamicRuntime(b, provider)

	call, err := message.NewToolCall("c1", "read_file", map[string]any{"file_path": "/tmp/x"})
	if err != nil {
		b.Fatalf("tool call: %v", err)
	}
	history := make([]message.Message, 0, 10_000)
	for i := 0; i < 10_000; i++ {
		if i%3 == 0 {
			history = append(history, message.Message{
				Role:    message.RoleAssistant,
				Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
			})
			continue
		}
		history = append(history, message.NewTextMessage(message.RoleUser, "turn"))
	}
	// The call answers a user turn; a history that ends with an assistant turn is
	// a configuration error the node now reports.
	history = append(history, message.NewTextMessage(message.RoleUser, "answer this"))
	snapshot := map[string]graph.Value{"messages": graph.MessagesValue(graph.MessagesFromSlice(history))}
	invoke := invokeFor(snapshot, `{"messages":"messages"}`)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		stream, err := nodes.Model(rt).Invoke(context.Background(), invoke)
		if err != nil {
			b.Fatalf("Invoke: %v", err)
		}
		drainNode(b, stream)
	}
}

// A transcript that ends with an assistant turn has nothing to answer; the node
// must say so instead of relabelling it as user input.
func TestModelRejectsAnAssistantTail(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "hi"),
			message.NewTextMessage(message.RoleAssistant, "hello"),
		})),
	}
	_, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot, `{"messages":"messages"}`))
	if err == nil || !strings.Contains(err.Error(), "only a user or tool turn") {
		t.Fatalf("err = %v, want a role complaint", err)
	}
}

// Tools apply to text output only: a media request must not carry them.
func TestModelSkipsToolsForNonTextOutput(t *testing.T) {
	provider := &stubProvider{response: textResponse("ignored")}
	rt := testRuntime(t, provider)
	snapshot := map[string]graph.Value{
		"messages": graph.MessagesValue(graph.MessagesFromSlice([]message.Message{
			message.NewTextMessage(message.RoleUser, "draw a cat"),
		})),
	}
	stream, err := nodes.Model(rt).Invoke(context.Background(), invokeFor(snapshot,
		`{"messages":"messages","request":{"output":"image"}}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	drainNode(t, stream)
	if got := provider.requests[0].Tools.Len(); got != 0 {
		t.Errorf("tools = %d for an image request, want none", got)
	}
	if got := provider.requests[0].Output; got != inference.OutputImage {
		t.Errorf("output = %q, want image", got)
	}
}

// A negative parallelism is a typo, not "unbounded".
func TestToolRejectsNegativeParallelism(t *testing.T) {
	rt := testRuntime(t, &stubProvider{})
	_, err := nodes.Tool(rt).Invoke(context.Background(), invokeFor(map[string]graph.Value{},
		`{"messages":"messages","pending":"pending","parallelism":-1}`))
	if err == nil || !strings.Contains(err.Error(), "parallelism") {
		t.Fatalf("err = %v, want a parallelism complaint", err)
	}
}
