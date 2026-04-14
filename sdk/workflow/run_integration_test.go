package workflow

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// --- mock helpers ---

type recordingSession struct {
	BaseSession
	history []model.Message
	vars    map[string]any
	loadErr error
	saveErr error

	loaded    atomic.Bool
	saved     atomic.Bool
	savedMsgs []model.Message
	closed    atomic.Bool
	closedErr error // runErr passed to Close
}

func (s *recordingSession) Load(_ context.Context) ([]model.Message, error) {
	s.loaded.Store(true)
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	cp := make([]model.Message, len(s.history))
	copy(cp, s.history)
	return cp, nil
}

func (s *recordingSession) Vars(_ context.Context) (map[string]any, error) { return s.vars, nil }

func (s *recordingSession) Save(_ context.Context, msgs []model.Message) error {
	s.saved.Store(true)
	s.savedMsgs = append([]model.Message(nil), msgs...)
	return s.saveErr
}

func (s *recordingSession) Close(_ context.Context, runErr error) error {
	s.closed.Store(true)
	s.closedErr = runErr
	return nil
}

type staticMemory struct {
	session *recordingSession
}

func (m *staticMemory) Session(_ context.Context, _ string) (MemorySession, error) {
	return m.session, nil
}

func newMemoryFactory(session *recordingSession) MemoryFactory {
	mem := &staticMemory{session: session}
	return func(_ context.Context, _ Agent) (Memory, error) { return mem, nil }
}

type echoStrategy struct {
	answer string
}

func (s echoStrategy) Kind() string                       { return "echo" }
func (s echoStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (s echoStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return echoRunnable{answer: s.answer}, nil
}

type echoRunnable struct{ answer string }

func (r echoRunnable) Execute(_ context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	reply := r.answer
	if reply == "" {
		reply = "echo"
	}
	board.AppendChannelMessage(MainChannel, model.NewTextMessage(model.RoleAssistant, reply))
	board.SetVar(VarAnswer, reply)
	return board, nil
}

type interruptStrategy struct{}

func (interruptStrategy) Kind() string                       { return "interrupt" }
func (interruptStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (interruptStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return interruptRunnable{}, nil
}

type interruptRunnable struct{}

func (interruptRunnable) Execute(_ context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	board.SetVar(VarInterruptedNode, "node_3")
	return board, errdefs.Interruptedf("paused at node_3")
}

type failStrategy struct{ err error }

func (s failStrategy) Kind() string                       { return "fail" }
func (s failStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (s failStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return failRunnable{err: s.err}, nil
}

type failRunnable struct{ err error }

func (r failRunnable) Execute(context.Context, *Board, *Request, ...RunOption) (*Board, error) {
	return nil, r.err
}

type cancelStrategy struct{}

func (cancelStrategy) Kind() string                       { return "cancel" }
func (cancelStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (cancelStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return cancelRunnable{}, nil
}

type cancelRunnable struct{}

func (cancelRunnable) Execute(ctx context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	return board, context.Canceled
}

type ctxAwareStrategy struct{}

func (ctxAwareStrategy) Kind() string                       { return "ctx-aware" }
func (ctxAwareStrategy) Capabilities() StrategyCapabilities { return StrategyCapabilities{} }
func (ctxAwareStrategy) Build(context.Context, *Dependencies) (Runnable, error) {
	return ctxAwareRunnable{}, nil
}

type ctxAwareRunnable struct{}

func (ctxAwareRunnable) Execute(ctx context.Context, board *Board, _ *Request, _ ...RunOption) (*Board, error) {
	if err := ctx.Err(); err != nil {
		return board, err
	}
	return board, nil
}

func testAgent(s Strategy) Agent {
	return NewAgent("test-agent", s)
}

// --- 2.8: full Memory path ---

func TestRuntime_FullMemoryPath(t *testing.T) {
	hist := []model.Message{
		model.NewTextMessage(model.RoleUser, "hello"),
		model.NewTextMessage(model.RoleAssistant, "hi there"),
	}
	sess := &recordingSession{
		history: hist,
		vars:    map[string]any{VarSummaryIndex: 1},
	}
	rt := NewRuntime(
		WithMemoryFactory(newMemoryFactory(sess)),
	)

	req := NewTextRequest("how are you?")
	req.ContextID = "conv-1"

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "fine"}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Load was called
	if !sess.loaded.Load() {
		t.Fatal("session.Load was not called")
	}
	// Save was called
	if !sess.saved.Load() {
		t.Fatal("session.Save was not called")
	}
	// Close was called with nil (success)
	if !sess.closed.Load() {
		t.Fatal("session.Close was not called")
	}
	if sess.closedErr != nil {
		t.Fatalf("session.Close got runErr=%v, want nil", sess.closedErr)
	}

	// Result should be completed
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", res.Status)
	}
	if res.State["answer"] != "fine" {
		t.Fatalf("answer=%v, want 'fine'", res.State["answer"])
	}

	// Messages should contain only the new messages (user input + assistant reply)
	if len(res.Messages) != 2 {
		t.Fatalf("len(Messages)=%d, want 2 (new user + assistant)", len(res.Messages))
	}
	if res.Messages[0].Content() != "how are you?" {
		t.Fatalf("Messages[0]=%q, want 'how are you?'", res.Messages[0].Content())
	}
	if res.Messages[1].Content() != "fine" {
		t.Fatalf("Messages[1]=%q, want 'fine'", res.Messages[1].Content())
	}

	// Save should have received the full message list (history + new)
	if len(sess.savedMsgs) != 4 {
		t.Fatalf("saved %d messages, want 4 (2 history + 1 user + 1 assistant)", len(sess.savedMsgs))
	}

	// Summary index should have been injected into the board
	if res.LastBoard == nil {
		t.Fatal("LastBoard is nil")
	}
	if v, ok := res.LastBoard.GetVar(VarSummaryIndex); !ok || v != 1 {
		t.Fatalf("summary_index=%v, want 1", v)
	}
}

func TestRuntime_MemoryLoadError(t *testing.T) {
	sess := &recordingSession{
		loadErr: errors.New("db connection lost"),
	}
	rt := NewRuntime(WithMemoryFactory(newMemoryFactory(sess)))
	req := NewTextRequest("hi")
	req.ContextID = "conv-1"

	_, err := rt.Run(context.Background(), testAgent(echoStrategy{}), req)
	if err == nil {
		t.Fatal("expected error from memory load")
	}
	if !sess.closed.Load() {
		t.Fatal("session.Close should still be called on load error")
	}
}

// --- 2.9: stateless path with WithHistory ---

func TestRuntime_StatelessWithHistory(t *testing.T) {
	hist := []model.Message{
		model.NewTextMessage(model.RoleUser, "first"),
		model.NewTextMessage(model.RoleAssistant, "response"),
	}
	rt := NewRuntime()
	req := NewTextRequest("second")

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "ok"}), req, WithHistory(hist))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
	// Messages should be only new ones (user + assistant)
	if len(res.Messages) != 2 {
		t.Fatalf("len(Messages)=%d, want 2", len(res.Messages))
	}
}

func TestRuntime_StatelessNoHistory(t *testing.T) {
	rt := NewRuntime()
	req := NewTextRequest("hello")

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "world"}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
	// All channel messages are new
	if len(res.Messages) != 2 {
		t.Fatalf("len(Messages)=%d, want 2", len(res.Messages))
	}
}

func TestRuntime_MemoryWinsOverWithHistory(t *testing.T) {
	sess := &recordingSession{
		history: []model.Message{model.NewTextMessage(model.RoleAssistant, "from memory")},
	}
	rt := NewRuntime(WithMemoryFactory(newMemoryFactory(sess)))
	req := NewTextRequest("hi")
	req.ContextID = "c1"

	externalHistory := []model.Message{model.NewTextMessage(model.RoleAssistant, "from caller")}

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "ok"}), req, WithHistory(externalHistory))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should contain memory history, NOT WithHistory
	if len(sess.savedMsgs) < 2 {
		t.Fatalf("expected saved messages to start with memory history")
	}
	if sess.savedMsgs[0].Content() != "from memory" {
		t.Fatalf("first saved message=%q, want 'from memory' (memory wins over WithHistory)", sess.savedMsgs[0].Content())
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
}

// --- 2.10: interrupt path ---

func TestRuntime_Interrupt(t *testing.T) {
	sess := &recordingSession{}
	rt := NewRuntime(WithMemoryFactory(newMemoryFactory(sess)))
	req := NewTextRequest("go")
	req.ContextID = "c-int"

	res, err := rt.Run(context.Background(), testAgent(interruptStrategy{}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusInterrupted {
		t.Fatalf("status=%s, want interrupted", res.Status)
	}
	if !errdefs.IsInterrupted(res.Err) {
		t.Fatalf("res.Err=%v, want interrupted error", res.Err)
	}
	if res.State["interrupted_node"] != "node_3" {
		t.Fatalf("interrupted_node=%v, want node_3", res.State["interrupted_node"])
	}
	if res.State["board"] == nil {
		t.Fatal("board snapshot missing from State on interrupt")
	}
	if sess.saved.Load() {
		t.Fatal("session.Save should NOT be called on interrupt")
	}
	if !sess.closed.Load() {
		t.Fatal("session.Close was not called")
	}
	if sess.closedErr == nil || !errdefs.IsInterrupted(sess.closedErr) {
		t.Fatalf("session.Close runErr=%v, want interrupted error", sess.closedErr)
	}
}

// --- 2.11: cancel path ---

func TestRuntime_ContextCanceled(t *testing.T) {
	sess := &recordingSession{}
	rt := NewRuntime(WithMemoryFactory(newMemoryFactory(sess)))
	req := NewTextRequest("go")
	req.ContextID = "c-cancel"

	res, err := rt.Run(context.Background(), testAgent(cancelStrategy{}), req)
	if err != nil {
		t.Fatalf("unexpected infrastructure error: %v", err)
	}
	if res.Status != StatusCanceled {
		t.Fatalf("status=%s, want canceled", res.Status)
	}
	if !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("res.Err=%v, want context.Canceled", res.Err)
	}
	if sess.saved.Load() {
		t.Fatal("session.Save should NOT be called on cancel")
	}
	if !sess.closed.Load() {
		t.Fatal("session.Close was not called")
	}
	if !errors.Is(sess.closedErr, context.Canceled) {
		t.Fatalf("session.Close runErr=%v, want context.Canceled", sess.closedErr)
	}
}

func TestRuntime_ContextDeadlineExceeded(t *testing.T) {
	rt := NewRuntime()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled

	agent := testAgent(ctxAwareStrategy{})
	req := NewTextRequest("hello")

	res, err := rt.Run(ctx, agent, req)
	if err != nil {
		t.Fatalf("unexpected infrastructure error: %v", err)
	}
	if res.Status != StatusCanceled {
		t.Fatalf("status=%s, want canceled", res.Status)
	}
}

// --- error path ---

func TestRuntime_ExecutionFailure(t *testing.T) {
	sess := &recordingSession{}
	rt := NewRuntime(WithMemoryFactory(newMemoryFactory(sess)))
	req := NewTextRequest("go")
	req.ContextID = "c-fail"
	execErr := errors.New("something broke")

	res, err := rt.Run(context.Background(), testAgent(failStrategy{err: execErr}), req)
	if err != nil {
		t.Fatalf("unexpected infrastructure error: %v", err)
	}
	if res.Status != StatusFailed {
		t.Fatalf("status=%s, want failed", res.Status)
	}
	if res.Err == nil || res.Err.Error() != "something broke" {
		t.Fatalf("res.Err=%v, want 'something broke'", res.Err)
	}
	if !sess.closed.Load() {
		t.Fatal("session.Close was not called")
	}
	if sess.closedErr == nil || sess.closedErr.Error() != "something broke" {
		t.Fatalf("session.Close runErr=%v, want 'something broke'", sess.closedErr)
	}
	if sess.saved.Load() {
		t.Fatal("session.Save should NOT be called on failure")
	}
}

func TestRuntime_AbortedPath(t *testing.T) {
	rt := NewRuntime()
	req := NewTextRequest("go")
	abortErr := errdefs.Abortedf("user aborted")

	res, err := rt.Run(context.Background(), testAgent(failStrategy{err: abortErr}), req)
	if err != nil {
		t.Fatalf("unexpected infrastructure error: %v", err)
	}
	if res.Status != StatusAborted {
		t.Fatalf("status=%s, want aborted", res.Status)
	}
	if !errdefs.IsAborted(res.Err) {
		t.Fatalf("res.Err=%v, want aborted error", res.Err)
	}
}

// --- nil guards ---

func TestRuntime_NilRequest(t *testing.T) {
	rt := NewRuntime()
	_, err := rt.Run(context.Background(), testAgent(echoStrategy{}), nil)
	if err == nil {
		t.Fatal("expected error for nil request")
	}
}

func TestRuntime_NilAgent(t *testing.T) {
	rt := NewRuntime()
	_, err := rt.Run(context.Background(), nil, NewTextRequest("hi"))
	if err == nil {
		t.Fatal("expected error for nil agent")
	}
}

func TestRuntime_NoMemoryFactory_NoContextID(t *testing.T) {
	sess := &recordingSession{}
	rt := NewRuntime(WithMemoryFactory(newMemoryFactory(sess)))
	req := NewTextRequest("hi")
	req.ContextID = "" // no context ID → skip memory

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "ok"}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
	if sess.loaded.Load() {
		t.Fatal("session.Load should not be called when contextID is empty")
	}
}

// --- stream callback passthrough ---

func TestRuntime_StreamCallbackOption(t *testing.T) {
	var captured []StreamEvent
	cb := func(e StreamEvent) { captured = append(captured, e) }

	rc := ApplyRunOpts([]RunOption{WithStreamCallback(cb)})
	if rc.StreamCallback == nil {
		t.Fatal("StreamCallback not set")
	}
	rc.StreamCallback(StreamEvent{Type: "token", NodeID: "n1", Payload: "hi"})
	if len(captured) != 1 || captured[0].Type != "token" {
		t.Fatalf("captured=%v", captured)
	}
}

func TestRuntime_MaxIterationsOption(t *testing.T) {
	rc := ApplyRunOpts([]RunOption{WithMaxIterations(50)})
	if rc.MaxIterations != 50 {
		t.Fatalf("MaxIterations=%d, want 50", rc.MaxIterations)
	}
}

// --- ContextAssembler / IncrementalSaver ---

type assemblingSession struct {
	BaseSession
	history      []model.Message
	vars         map[string]any
	assembled    atomic.Bool
	assembledReq *Request
}

func (s *assemblingSession) Assemble(_ context.Context, req *Request) ([]model.Message, error) {
	s.assembled.Store(true)
	s.assembledReq = req
	cp := make([]model.Message, len(s.history))
	copy(cp, s.history)
	return cp, nil
}

func (s *assemblingSession) Vars(_ context.Context) (map[string]any, error) { return s.vars, nil }
func (s *assemblingSession) Close(_ context.Context, _ error) error         { return nil }

type assemblingMemory struct{ session *assemblingSession }

func (m *assemblingMemory) Session(_ context.Context, _ string) (MemorySession, error) {
	return m.session, nil
}

func TestRuntime_ContextAssembler(t *testing.T) {
	sess := &assemblingSession{
		history: []model.Message{
			model.NewTextMessage(model.RoleUser, "summarized context"),
		},
		vars: map[string]any{VarSummaryIndex: 1},
	}
	factory := func(_ context.Context, _ Agent) (Memory, error) {
		return &assemblingMemory{session: sess}, nil
	}
	rt := NewRuntime(WithMemoryFactory(factory))

	req := NewTextRequest("new question")
	req.ContextID = "conv-asm"

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "assembled"}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sess.assembled.Load() {
		t.Fatal("Assemble was not called")
	}
	if sess.assembledReq != req {
		t.Fatal("Assemble did not receive the original request")
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", res.Status)
	}
	if len(res.Messages) != 2 {
		t.Fatalf("len(Messages)=%d, want 2 (user + assistant)", len(res.Messages))
	}
	if res.Messages[0].Content() != "new question" {
		t.Fatalf("Messages[0]=%q, want 'new question'", res.Messages[0].Content())
	}
}

type incrementalSession struct {
	BaseSession
	history     []model.Message
	appended    atomic.Bool
	appendedMsgs []model.Message
}

func (s *incrementalSession) Load(_ context.Context) ([]model.Message, error) {
	cp := make([]model.Message, len(s.history))
	copy(cp, s.history)
	return cp, nil
}

func (s *incrementalSession) Append(_ context.Context, msgs []model.Message) error {
	s.appended.Store(true)
	s.appendedMsgs = append([]model.Message(nil), msgs...)
	return nil
}

func (s *incrementalSession) Close(_ context.Context, _ error) error { return nil }

type incrementalMemory struct{ session *incrementalSession }

func (m *incrementalMemory) Session(_ context.Context, _ string) (MemorySession, error) {
	return m.session, nil
}

func TestRuntime_IncrementalSaver(t *testing.T) {
	sess := &incrementalSession{
		history: []model.Message{
			model.NewTextMessage(model.RoleUser, "old"),
			model.NewTextMessage(model.RoleAssistant, "old reply"),
		},
	}
	factory := func(_ context.Context, _ Agent) (Memory, error) {
		return &incrementalMemory{session: sess}, nil
	}
	rt := NewRuntime(WithMemoryFactory(factory))

	req := NewTextRequest("new")
	req.ContextID = "conv-inc"

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "new reply"}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sess.appended.Load() {
		t.Fatal("Append was not called")
	}
	if len(sess.appendedMsgs) != 2 {
		t.Fatalf("appended %d messages, want 2 (new user + assistant)", len(sess.appendedMsgs))
	}
	if sess.appendedMsgs[0].Content() != "new" {
		t.Fatalf("appendedMsgs[0]=%q, want 'new'", sess.appendedMsgs[0].Content())
	}
	if sess.appendedMsgs[1].Content() != "new reply" {
		t.Fatalf("appendedMsgs[1]=%q, want 'new reply'", sess.appendedMsgs[1].Content())
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s, want completed", res.Status)
	}
}

type fullComboSession struct {
	BaseSession
	history      []model.Message
	vars         map[string]any
	assembled    atomic.Bool
	appended     atomic.Bool
	appendedMsgs []model.Message
}

func (s *fullComboSession) Assemble(_ context.Context, _ *Request) ([]model.Message, error) {
	s.assembled.Store(true)
	cp := make([]model.Message, len(s.history))
	copy(cp, s.history)
	return cp, nil
}

func (s *fullComboSession) Append(_ context.Context, msgs []model.Message) error {
	s.appended.Store(true)
	s.appendedMsgs = append([]model.Message(nil), msgs...)
	return nil
}

func (s *fullComboSession) Vars(_ context.Context) (map[string]any, error) { return s.vars, nil }
func (s *fullComboSession) Close(_ context.Context, _ error) error         { return nil }

type fullComboMemory struct{ session *fullComboSession }

func (m *fullComboMemory) Session(_ context.Context, _ string) (MemorySession, error) {
	return m.session, nil
}

func TestRuntime_ContextAssembler_And_IncrementalSaver(t *testing.T) {
	sess := &fullComboSession{
		history: []model.Message{
			model.NewTextMessage(model.RoleUser, "ctx"),
		},
	}
	factory := func(_ context.Context, _ Agent) (Memory, error) {
		return &fullComboMemory{session: sess}, nil
	}
	rt := NewRuntime(WithMemoryFactory(factory))

	req := NewTextRequest("hello")
	req.ContextID = "conv-combo"

	res, err := rt.Run(context.Background(), testAgent(echoStrategy{answer: "world"}), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !sess.assembled.Load() {
		t.Fatal("Assemble was not called")
	}
	if !sess.appended.Load() {
		t.Fatal("Append was not called")
	}
	if len(sess.appendedMsgs) != 2 {
		t.Fatalf("appended %d messages, want 2", len(sess.appendedMsgs))
	}
	if res.Status != StatusCompleted {
		t.Fatalf("status=%s", res.Status)
	}
}
