package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
)

// fakeStream replays one canned round.
type fakeStream struct {
	events []inference.StreamEvent
	resp   inference.GenerateResponse
}

func (s *fakeStream) Next(context.Context) (inference.StreamEvent, error) {
	if len(s.events) == 0 {
		return inference.StreamEvent{}, io.EOF
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *fakeStream) Result() (inference.GenerateResponse, error) { return s.resp, nil }

func (s *fakeStream) Close() error { return nil }

// scriptedProvider answers each round from a list.
type scriptedProvider struct {
	steps []inference.GenerateResponse
	texts []string
	seen  []inference.GenerateRequest
}

func (p *scriptedProvider) GenerateStream(_ context.Context, request inference.GenerateRequest) (inference.Stream, error) {
	if len(p.steps) == 0 {
		return nil, errors.New("scripted provider ran out of steps")
	}
	p.seen = append(p.seen, request)
	step := p.steps[0]
	p.steps = p.steps[1:]
	var events []inference.StreamEvent
	if len(p.texts) > 0 {
		events = append(events, inference.StreamEvent{Kind: inference.StreamText, Text: p.texts[0]})
		p.texts = p.texts[1:]
	}
	return &fakeStream{events: events, resp: step}, nil
}

func (p *scriptedProvider) Generate(ctx context.Context, request inference.GenerateRequest) (inference.GenerateResponse, error) {
	stream, err := p.GenerateStream(ctx, request)
	if err != nil {
		return inference.GenerateResponse{}, err
	}
	for {
		if _, err := stream.Next(ctx); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return inference.GenerateResponse{}, err
		}
	}
	return stream.Result()
}

func toolCallResponse(t *testing.T, call message.ToolCall) inference.GenerateResponse {
	t.Helper()
	return inference.GenerateResponse{Message: message.Message{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
	}}
}

func textResponse(text string) inference.GenerateResponse {
	return inference.GenerateResponse{Message: message.NewTextMessage(message.RoleAssistant, text)}
}

func newTestSession(t *testing.T, provider *scriptedProvider) (*Session, *runtime.Runtime) {
	t.Helper()
	root := t.TempDir()
	cfg := runtime.Config{
		Version: 1,
		Sandbox: runtime.SandboxConfig{Backend: runtime.BackendLocal, Writes: []string{root}},
		Files:   runtime.FilesConfig{Roots: []string{root}},
		Inference: runtime.InferenceConfig{
			Providers: []runtime.ProviderConfig{{Name: "p", Driver: "stub"}},
			Models:    []runtime.ModelConfig{{Provider: "p", Model: "m"}},
		},
	}
	rt, err := runtime.New(context.Background(), cfg, runtime.WithDrivers(runtime.Driver{
		Name: "stub",
		Build: func(map[string]any) (inference.Provider, error) {
			return inference.Provider{Generator: provider}, nil
		},
	}))
	if err != nil {
		t.Fatalf("runtime.New: %v", err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	session, err := NewSession(rt)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return session, rt
}

// One Run with a tool call: the loop must execute the tool, feed the result
// back, and finish with the model's second answer.
func TestRunExecutesOneToolCall(t *testing.T) {
	_, rt := newTestSession(t, &scriptedProvider{})
	path := filepath.Join(rt.Files().Writable()[0], "doc.txt")
	if err := rt.Files().Write(context.Background(), path, []byte("hello\n")); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	call, err := message.NewToolCall("call-1", "read_file", map[string]any{"file_path": path})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	provider := &scriptedProvider{
		steps: []inference.GenerateResponse{toolCallResponse(t, call), textResponse("done")},
		texts: []string{"reading"},
	}
	session, _ := newTestSession(t, provider)

	ctx := context.Background()
	var kinds []EventKind
	var resultContent string
	for event, err := range session.Run(ctx, Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "what does doc.txt say?"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		kinds = append(kinds, event.Kind())
		if tr, ok := event.(ToolResult); ok {
			// read_file answers with a JSON envelope, not the bare bytes.
			var payload map[string]any
			if err := json.Unmarshal([]byte(tr.Result.Content.Text()), &payload); err == nil {
				if text, ok := payload["content"].(string); ok {
					resultContent = text
				}
			}
		}
	}

	want := []EventKind{KindStatus, KindStreamStart, KindText, KindUsage, KindToolCall,
		KindToolResult, KindStreamStart, KindUsage, KindStatus}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
	if resultContent == "" || resultContent[:5] != "hello" {
		t.Errorf("tool result = %q, want the file content", resultContent)
	}
	task := session.Task()
	if task.State != StateCompleted {
		t.Errorf("state = %s, want completed", task.State)
	}
	if task.Round != 1 {
		t.Errorf("round = %d, want 1", task.Round)
	}
	if len(task.History) != 4 {
		t.Errorf("history = %d messages, want 4", len(task.History))
	}
	// The second round must have carried the tool result back to the model.
	if len(provider.seen) != 2 {
		t.Fatalf("provider calls = %d, want 2", len(provider.seen))
	}
	if provider.seen[1].Input.Role != inference.InputRoleTool {
		t.Errorf("second round input role = %q, want tool", provider.seen[1].Input.Role)
	}
}

// A model answer with no tool calls finishes in one round.
func TestRunWithoutToolsFinishesImmediately(t *testing.T) {
	provider := &scriptedProvider{steps: []inference.GenerateResponse{textResponse("hi")}}
	session, _ := newTestSession(t, provider)

	var last TaskState
	for event, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "hello"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if status, ok := event.(Status); ok {
			last = status.State
		}
	}
	if last != StateCompleted {
		t.Errorf("final state = %s, want completed", last)
	}
	if session.Task().Round != 0 {
		t.Errorf("round = %d, want 0", session.Task().Round)
	}
}

func TestRunRejectsEmptyInput(t *testing.T) {
	provider := &scriptedProvider{}
	session, _ := newTestSession(t, provider)
	for _, err := range session.Run(context.Background(), Input{}) {
		if err == nil {
			t.Fatal("empty input accepted")
		}
		return
	}
}

// Visibility is a function of history: a tool called in a previous Task is
// visible again without the caller passing anything.
func TestReplayDiscoveriesRestoresVisibility(t *testing.T) {
	provider := &scriptedProvider{}
	session, _ := newTestSession(t, provider)
	call, err := message.NewToolCall("c1", "read_file", map[string]any{"file_path": "/tmp/x"})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	session.Task().History = []message.Message{{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
	}}
	view := session.runtime.Assembly().NewCatalogView()
	replayDiscoveries(view, session.Task().History)
	names := make([]string, 0, len(view.Definitions()))
	for _, definition := range view.Definitions() {
		names = append(names, definition.Name)
	}
	found := false
	for _, name := range names {
		if name == "read_file" {
			found = true
		}
	}
	if !found {
		t.Errorf("definitions = %v, want read_file restored from history", names)
	}
}
