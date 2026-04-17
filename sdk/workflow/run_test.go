package workflow

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

type noopStrategy struct{}

func (noopStrategy) Kind() string { return "noop" }
func (noopStrategy) Capabilities() StrategyCapabilities {
	return StrategyCapabilities{}
}
func (noopStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return noopRunnable{}, nil
}

type noopRunnable struct{}

func (noopRunnable) Execute(ctx context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	board.SetVar(VarAnswer, "ok")
	return board, nil
}

type noopAgent struct {
	str Strategy
}

func (a noopAgent) ID() string         { return "a1" }
func (a noopAgent) Card() AgentCard    { return AgentCard{Name: "a1"} }
func (a noopAgent) Strategy() Strategy { return a.str }
func (a noopAgent) Tools() []string    { return nil }

func TestRuntime_Run_NoMemory_Completed(t *testing.T) {
	rt := NewRuntime()
	req := NewTextRequest("hello")
	req.ContextID = ""
	res, err := rt.Run(context.Background(), noopAgent{str: noopStrategy{}}, req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
	if res.State["answer"] != "ok" {
		t.Fatalf("answer=%v", res.State["answer"])
	}
}

func TestNewTextRequest(t *testing.T) {
	r := NewTextRequest("hi")
	if MessageText(r.Message) != "hi" {
		t.Fatal("message text mismatch")
	}
	if r.Message.Role != model.RoleUser {
		t.Fatal("expected user role")
	}
}

// --- Result.Text ---

func TestResult_Text(t *testing.T) {
	r := &Result{
		Messages: []model.Message{
			model.NewTextMessage(model.RoleUser, "hello"),
			model.NewTextMessage(model.RoleAssistant, "world"),
		},
	}
	if r.Text() != "world" {
		t.Fatalf("expected 'world', got %q", r.Text())
	}
}

func TestResult_Text_Nil(t *testing.T) {
	var r *Result
	if r.Text() != "" {
		t.Fatal("expected empty for nil result")
	}
}

func TestResult_Text_NoAssistant(t *testing.T) {
	r := &Result{
		Messages: []model.Message{
			model.NewTextMessage(model.RoleUser, "hello"),
		},
	}
	if r.Text() != "" {
		t.Fatalf("expected empty when no assistant message, got %q", r.Text())
	}
}

func TestResult_Text_EmptyMessages(t *testing.T) {
	r := &Result{}
	if r.Text() != "" {
		t.Fatal("expected empty for no messages")
	}
}

// --- MessageText non-user ---

func TestMessageText_NonUser(t *testing.T) {
	msg := model.NewTextMessage(model.RoleAssistant, "reply")
	if MessageText(msg) != "" {
		t.Fatal("expected empty for non-user role")
	}
}

// --- Agent accessors ---

func TestNewAgent_WithOptions(t *testing.T) {
	a := NewAgent("test", noopStrategy{},
		WithAgentDescription("desc"),
		WithAgentTools([]string{"tool1", "tool2"}),
	)

	if a.ID() != "test" {
		t.Fatalf("ID=%q", a.ID())
	}
	if a.Card().Description != "desc" {
		t.Fatalf("Description=%q", a.Card().Description)
	}
	if len(a.Tools()) != 2 {
		t.Fatalf("Tools=%v", a.Tools())
	}
}

// --- BaseSession ---

func TestBaseSession(t *testing.T) {
	ctx := context.Background()
	s := BaseSession{}

	msgs, err := s.Load(ctx)
	if err != nil || msgs != nil {
		t.Fatalf("Load: msgs=%v err=%v", msgs, err)
	}

	vars, err := s.Vars(ctx)
	if err != nil || vars != nil {
		t.Fatalf("Vars: vars=%v err=%v", vars, err)
	}

	if err := s.Save(ctx, nil); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Close(ctx, nil); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// --- WithDependencies ---

func TestRuntime_WithDependencies(t *testing.T) {
	deps := NewDependencies()
	SetDep(deps, "key", "val")

	var captured *Dependencies
	strategy := &captureDepsStrategy{capture: &captured}

	rt := NewRuntime(WithDependencies(deps))
	req := NewTextRequest("hi")
	_, err := rt.Run(context.Background(), NewAgent("a", strategy), req)
	if err != nil {
		t.Fatal(err)
	}
	if captured == nil {
		t.Fatal("deps not captured")
	}
	v, err := GetDep[string](captured, "key")
	if err != nil || v != "val" {
		t.Fatalf("expected 'val', got %q err=%v", v, err)
	}
}

type captureDepsStrategy struct {
	capture **Dependencies
}

func (s *captureDepsStrategy) Kind() string                       { return "capture" }
func (s *captureDepsStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (s *captureDepsStrategy) Build(_ context.Context, deps *Dependencies) (Runnable, error) {
	*s.capture = deps
	return noopRunnable{}, nil
}

// --- WithPrepareBoard ---

func TestRuntime_WithPrepareBoard(t *testing.T) {
	called := false
	rt := NewRuntime(WithPrepareBoard(func(ctx context.Context, agent Agent, req *Request, session MemorySession, opts []RunOption) (*Board, error) {
		called = true
		b := NewBoard()
		b.AppendChannelMessage(MainChannel, req.Message)
		b.SetVar(VarPrevMessageCount, 0)
		b.SetVar(VarRunID, "custom-run")
		return b, nil
	}))

	req := NewTextRequest("hi")
	_, err := rt.Run(context.Background(), NewAgent("a", noopStrategy{}), req)
	if err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("custom prepareBoardFn was not called")
	}
}

// --- Memory save error ---

func TestRuntime_MemorySaveError(t *testing.T) {
	sess := &savingErrorSession{saveErr: errors.New("disk full")}
	factory := func(_ context.Context, _ Agent) (Memory, error) {
		return &staticMem{session: sess}, nil
	}
	rt := NewRuntime(WithMemoryFactory(factory))
	req := NewTextRequest("hi")
	req.ContextID = "c1"

	_, err := rt.Run(context.Background(), NewAgent("a", echoStrategy2{answer: "ok"}), req)
	if err == nil {
		t.Fatal("expected error from memory save")
	}
}

type savingErrorSession struct {
	BaseSession
	saveErr error
}

func (s *savingErrorSession) Load(_ context.Context) ([]model.Message, error) { return nil, nil }
func (s *savingErrorSession) Vars(_ context.Context) (map[string]any, error)  { return nil, nil }
func (s *savingErrorSession) Save(_ context.Context, _ []model.Message) error { return s.saveErr }
func (s *savingErrorSession) Close(_ context.Context, _ error) error          { return nil }

type staticMem struct{ session MemorySession }

func (m *staticMem) Session(_ context.Context, _ string) (MemorySession, error) {
	return m.session, nil
}

type echoStrategy2 struct{ answer string }

func (s echoStrategy2) Kind() string                       { return "echo" }
func (s echoStrategy2) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (s echoStrategy2) Build(context.Context, *Dependencies) (Runnable, error) {
	return echoRunnable2{answer: s.answer}, nil
}

type echoRunnable2 struct{ answer string }

func (r echoRunnable2) Execute(_ context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	board.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleAssistant, r.answer))
	board.SetVar(VarAnswer, r.answer)
	return board, nil
}

// --- Run with custom RunID and RuntimeID ---

func TestRuntime_RequestRunIDAndRuntimeID(t *testing.T) {
	rt := NewRuntime()
	req := NewTextRequest("hi")
	req.RunID = "my-run-id"
	req.RuntimeID = "my-runtime"

	res, err := rt.Run(context.Background(), NewAgent("a", noopStrategy{}), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.State["run_id"] != "my-run-id" {
		t.Fatalf("run_id=%v, want my-run-id", res.State["run_id"])
	}
}

// --- Usage propagation ---

func TestRuntime_UsagePropagation(t *testing.T) {
	rt := NewRuntime()
	strategy := usageStrategy{usage: model.TokenUsage{InputTokens: 10, OutputTokens: 20}}
	req := NewTextRequest("hi")

	res, err := rt.Run(context.Background(), NewAgent("a", strategy), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Usage.InputTokens != 10 || res.Usage.OutputTokens != 20 {
		t.Fatalf("usage=%+v", res.Usage)
	}
}

type usageStrategy struct{ usage model.TokenUsage }

func (s usageStrategy) Kind() string                       { return "usage" }
func (s usageStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (s usageStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return usageRunnable{usage: s.usage}, nil
}

type usageRunnable struct{ usage model.TokenUsage }

func (r usageRunnable) Execute(_ context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	board.SetVar(VarInternalUsage, r.usage)
	board.SetVar(VarAnswer, "done")
	return board, nil
}

// --- WithBoard (resume) ---

func TestRuntime_WithBoard_SkipsPrepareBoard(t *testing.T) {
	board := NewBoard()
	board.SetVar(VarRunID, "resumed-run")
	board.SetVar(VarPrevMessageCount, 0)
	board.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleUser, "original"))

	rt := NewRuntime()
	req := NewTextRequest("resume-input")
	res, err := rt.Run(context.Background(), noopAgent{str: noopStrategy{}}, req, WithBoard(board))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
	if res.State["run_id"] != "resumed-run" {
		t.Fatalf("run_id=%v, want resumed-run", res.State["run_id"])
	}
}

func TestRuntime_WithBoard_OverridesCustomPrepareBoard(t *testing.T) {
	prepareCalled := false
	rt := NewRuntime(WithPrepareBoard(func(_ context.Context, _ Agent, _ *Request, _ MemorySession, _ []RunOption) (*Board, error) {
		prepareCalled = true
		return NewBoard(), nil
	}))

	board := NewBoard()
	board.SetVar(VarRunID, "injected")
	board.SetVar(VarPrevMessageCount, 0)

	req := NewTextRequest("hi")
	res, err := rt.Run(context.Background(), noopAgent{str: noopStrategy{}}, req, WithBoard(board))
	if err != nil {
		t.Fatal(err)
	}
	if prepareCalled {
		t.Fatal("prepareBoardFn should be skipped when WithBoard is used")
	}
	if res.State["run_id"] != "injected" {
		t.Fatalf("run_id=%v, want injected", res.State["run_id"])
	}
}

func TestRuntime_WithBoard_PreservesExistingChannelMessages(t *testing.T) {
	board := NewBoard()
	board.SetVar(VarRunID, "r1")
	board.SetVar(VarPrevMessageCount, 2)
	board.SetChannel(MainChannel, []model.Message{
		model.NewTextMessage(model.RoleUser, "turn1"),
		model.NewTextMessage(model.RoleAssistant, "reply1"),
	})

	rt := NewRuntime()
	req := NewTextRequest("turn2")
	res, err := rt.Run(context.Background(), noopAgent{str: echoStrategy2{answer: "reply2"}}, req, WithBoard(board))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
	if len(res.Messages) != 1 {
		t.Fatalf("expected 1 new message, got %d", len(res.Messages))
	}
	if res.Messages[0].Content() != "reply2" {
		t.Fatalf("message=%q, want reply2", res.Messages[0].Content())
	}
}

func TestRuntime_WithBoard_MemorySessionStillSaves(t *testing.T) {
	saved := false
	sess := &trackingSaveSession{onSave: func() { saved = true }}
	factory := func(_ context.Context, _ Agent) (Memory, error) {
		return &staticMem{session: sess}, nil
	}
	rt := NewRuntime(WithMemoryFactory(factory))

	board := NewBoard()
	board.SetVar(VarRunID, "resume")
	board.SetVar(VarPrevMessageCount, 0)
	board.SetChannel(MainChannel, []model.Message{})

	req := NewTextRequest("hi")
	req.ContextID = "c1"
	_, err := rt.Run(context.Background(), noopAgent{str: echoStrategy2{answer: "ok"}}, req, WithBoard(board))
	if err != nil {
		t.Fatal(err)
	}
	if !saved {
		t.Fatal("memory session should still save after WithBoard resume")
	}
}

type trackingSaveSession struct {
	BaseSession
	onSave func()
}

func (s *trackingSaveSession) Load(context.Context) ([]model.Message, error) { return nil, nil }
func (s *trackingSaveSession) Vars(context.Context) (map[string]any, error)  { return nil, nil }
func (s *trackingSaveSession) Save(_ context.Context, _ []model.Message) error {
	s.onSave()
	return nil
}
func (s *trackingSaveSession) Close(_ context.Context, _ error) error { return nil }
