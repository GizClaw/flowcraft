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

func newTestSession(t testing.TB, provider inference.Generator, opts ...Option) (*Session, *runtime.Runtime) {
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
	session, err := NewSession(rt, opts...)
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

	// The second round's answer arrives without deltas, so it is emitted from
	// the response message: the caller must see the text either way.
	want := []EventKind{KindStatus, KindStreamStart, KindText, KindUsage, KindToolCall,
		KindToolResult, KindStreamStart, KindText, KindUsage, KindStatus}
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
	seedHistory(t, session, []message.Message{{
		Role:    message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{message.ToolCallPart{Call: call}}},
	}})
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

// memStore is a checkpoint store that keeps the newest value in memory.
type memStore struct {
	latest Checkpoint
	events map[string][]EventRecord
	saves  int
}

func (s *memStore) Save(_ context.Context, checkpoint Checkpoint, events []EventRecord) error {
	s.latest = checkpoint
	s.saves++
	if s.events == nil {
		s.events = make(map[string][]EventRecord)
	}
	s.events[checkpoint.Task.ID] = append(s.events[checkpoint.Task.ID], events...)
	return nil
}

func (s *memStore) Load(_ context.Context, taskID string) (Checkpoint, error) {
	if s.saves == 0 || s.latest.Task.ID != taskID {
		return Checkpoint{}, ErrNoCheckpoint
	}
	return s.latest, nil
}

func (s *memStore) LatestForContext(_ context.Context, conversationID string) (Checkpoint, error) {
	if s.saves == 0 || s.latest.Task.ContextID != conversationID {
		return Checkpoint{}, ErrNoCheckpoint
	}
	return s.latest, nil
}

func (s *memStore) Events(_ context.Context, taskID string) ([]EventRecord, error) {
	return s.events[taskID], nil
}

func (s *memStore) List(context.Context) ([]TaskSummary, error) {
	if s.saves == 0 {
		return nil, nil
	}
	return []TaskSummary{{ID: s.latest.Task.ID, State: s.latest.Task.State}}, nil
}

func (s *memStore) Delete(context.Context, string) error { return nil }

func (s *memStore) PutBlob(_ context.Context, data []byte) (string, error) {
	return string(data), nil
}

func (s *memStore) GetBlob(_ context.Context, ref string) ([]byte, error) {
	return []byte(ref), nil
}

func askCall(t *testing.T, question string) message.ToolCall {
	t.Helper()
	call, err := message.NewToolCall("ask-1", askUserTool, map[string]any{"question": question})
	if err != nil {
		t.Fatalf("tool call: %v", err)
	}
	return call
}

// A host that can answer immediately keeps the Task running: the reply comes
// back as the tool result and the loop continues.
func TestAskSynchronouslyContinues(t *testing.T) {
	provider := &scriptedProvider{
		steps: []inference.GenerateResponse{
			toolCallResponse(t, askCall(t, "which file?")),
			textResponse("done"),
		},
	}
	session, _ := newTestSession(t, provider, WithAsk(
		func(context.Context, Prompt) (Reply, error) { return Reply{Text: "a.txt"}, nil }))

	var result string
	for event, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "read something"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if tr, ok := event.(ToolResult); ok {
			result = tr.Result.Content.Text()
		}
	}
	if result != "a.txt" {
		t.Errorf("tool result = %q, want the reply", result)
	}
	if session.Task().State != StateCompleted {
		t.Errorf("state = %s, want completed", session.Task().State)
	}
}

// A host that cannot answer yet pauses the Task: the state is input-required, the
// pending call is recorded, and a checkpoint is written.
func TestAskPausesAndResumes(t *testing.T) {
	provider := &scriptedProvider{
		steps: []inference.GenerateResponse{
			toolCallResponse(t, askCall(t, "which file?")),
			textResponse("read a.txt"),
		},
	}
	store := &memStore{}
	session, _ := newTestSession(t, provider,
		WithAsk(func(context.Context, Prompt) (Reply, error) { return Reply{}, ErrPaused }),
		WithStore(store))

	for _, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "read something"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	task := session.Task()
	if task.State != StateInputRequired {
		t.Fatalf("state = %s, want input-required", task.State)
	}
	if task.Pending == nil || task.Pending.ID != "ask-1" {
		t.Fatalf("pending = %+v, want the ask_user call", task.Pending)
	}
	// Two writes: the seed at Run start, then the pause. The point of the count
	// is that the loop does not save per round, not that it never seeds.
	if store.saves != 2 {
		t.Errorf("checkpoint saves = %d, want 2 (seed + pause)", store.saves)
	}
	if store.latest.Task.ID != task.ID || store.latest.Task.Pending == nil {
		t.Errorf("checkpoint task = %+v, want the paused task", store.latest.Task)
	}

	var continued bool
	for event, err := range session.Resume(context.Background(), Reply{Text: "a.txt"}) {
		if err != nil {
			t.Fatalf("resume: %v", err)
		}
		if tr, ok := event.(ToolResult); ok && tr.Result.Content.Text() == "a.txt" {
			continued = true
		}
	}
	if !continued {
		t.Error("the reply did not become the pending call's result")
	}
	if session.Task().State != StateCompleted {
		t.Errorf("state = %s, want completed", session.Task().State)
	}
	if session.Task().Pending != nil {
		t.Error("pending call survived the resume")
	}
}

// Without an Ask seam the tool is not offered, and a model that calls it anyway
// gets a result it can act on instead of a dead Task.
func TestAskUnsupportedIsAToolError(t *testing.T) {
	provider := &scriptedProvider{
		steps: []inference.GenerateResponse{
			toolCallResponse(t, askCall(t, "which file?")),
			textResponse("done anyway"),
		},
	}
	session, _ := newTestSession(t, provider)
	for _, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "go"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	}
	if session.Task().State != StateCompleted {
		t.Errorf("state = %s, want completed", session.Task().State)
	}
}

// A driver that answers without streaming still reaches the caller: the text is
// in the response message, and an empty event stream would lose it.
func TestFinalTextIsEmittedWithoutDeltas(t *testing.T) {
	provider := &scriptedProvider{steps: []inference.GenerateResponse{textResponse("quiet answer")}}
	session, _ := newTestSession(t, provider)

	var text string
	for event, err := range session.Run(context.Background(), Input{Messages: []message.Message{
		message.NewTextMessage(message.RoleUser, "hi"),
	}}) {
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if chunk, ok := event.(Text); ok {
			text += chunk.Text
		}
	}
	if text != "quiet answer" {
		t.Errorf("text = %q, want the response message", text)
	}
}

// The ask_user schema never varies, so building it per round is pure waste.
func TestAskUserDefinitionAllocatesNothing(t *testing.T) {
	if got := testing.AllocsPerRun(100, func() { _ = askUserDefinition() }); got != 0 {
		t.Fatalf("askUserDefinition allocates %.0f times, want 0", got)
	}
}

func BenchmarkAskUserDefinition(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = askUserDefinition()
	}
}

// NewToolset validates and clones every schema; a round that has not changed
// the visible set must reuse the previous result.
func TestToolsetIsReusedWhileVisibilityIsUnchanged(t *testing.T) {
	session, _ := newTestSession(t, &scriptedProvider{})
	view := session.runtime.Assembly().NewCatalogView()
	if _, err := session.toolsFor(view); err != nil {
		t.Fatalf("toolsFor: %v", err)
	}
	calls := func() { _, _ = session.toolsFor(view) }
	if got := testing.AllocsPerRun(100, calls); got != 0 {
		t.Fatalf("toolsFor allocates %.0f times on the reuse path, want 0", got)
	}
}

func BenchmarkToolsFor(b *testing.B) {
	session, _ := newTestSession(b, &scriptedProvider{})
	view := session.runtime.Assembly().NewCatalogView()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := session.toolsFor(view); err != nil {
			b.Fatal(err)
		}
	}
}

// Two saves that would write the same row cost a round trip each; events always
// write, because they advance the log.
func TestUnchangedSavesAreSkipped(t *testing.T) {
	store := &memStore{}
	session, _ := newTestSession(t, &scriptedProvider{}, WithStore(store))
	ctx := context.Background()

	if err := session.persist(ctx, nil); err != nil {
		t.Fatalf("persist: %v", err)
	}
	wrote := store.saves
	if err := session.persist(ctx, nil); err != nil {
		t.Fatalf("persist again: %v", err)
	}
	if store.saves != wrote {
		t.Errorf("second identical save wrote %d rows, want 0 more", store.saves-wrote)
	}
	events := []EventRecord{messageEvent(message.NewTextMessage(message.RoleAssistant, "hi"))}
	if err := session.persist(ctx, events); err != nil {
		t.Fatalf("persist with events: %v", err)
	}
	if store.saves != wrote+1 {
		t.Errorf("save with events = %d writes, want one", store.saves-wrote)
	}
}
