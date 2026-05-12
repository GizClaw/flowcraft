package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// completedEngine appends a single assistant reply and returns nil.
// Used as the "happy path" engine across run tests.
func completedEngine(reply string) engine.Engine {
	return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, reply))
		return b, nil
	})
}

func newReq(text string) agent.Request {
	return agent.Request{Message: model.NewTextMessage(model.RoleUser, text)}
}

func TestRun_NilEngineRejected(t *testing.T) {
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, nil, newReq("hi"))
	if err == nil {
		t.Fatal("expected error for nil engine")
	}
	if res != nil {
		t.Errorf("expected nil result on infrastructure error, got %+v", res)
	}
}

func TestRun_EmptyAgentIDRejected(t *testing.T) {
	res, err := agent.Run(context.Background(), agent.Agent{}, completedEngine("ok"), newReq("hi"))
	if err == nil {
		t.Fatal("expected error for empty Agent.ID")
	}
	if res != nil {
		t.Errorf("expected nil result on infrastructure error, got %+v", res)
	}
}

func TestRun_CleanCompletion_Defaults(t *testing.T) {
	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "a"}, completedEngine("hi back"), newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Errorf("Status = %q, want completed", res.Status)
	}
	if !res.Committed {
		t.Error("StatusCompleted should default to Committed=true")
	}
	if got := res.Text(); got != "hi back" {
		t.Errorf("Text = %q, want %q", got, "hi back")
	}
	if res.RunID == "" {
		t.Error("RunID should be auto-generated when req.RunID is empty")
	}
	if !strings.HasPrefix(res.RunID, "run-") {
		t.Errorf("auto-generated RunID lacks expected prefix: %q", res.RunID)
	}
	if res.LastBoard == nil {
		t.Error("LastBoard should never be nil")
	}
}

func TestRun_RunIDPropagatesIntoEngineRun(t *testing.T) {
	var seen string
	eng := engine.EngineFunc(func(_ context.Context, r engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		seen = r.ID
		return b, nil
	})

	req := newReq("hi")
	req.RunID = "run-explicit-42"

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != "run-explicit-42" {
		t.Errorf("engine.Run.ID = %q, want propagation of req.RunID", seen)
	}
	if res.RunID != "run-explicit-42" {
		t.Errorf("Result.RunID = %q, want propagation of req.RunID", res.RunID)
	}
}

func TestRun_AttributesContainWellKnownKeys(t *testing.T) {
	var got map[string]string
	eng := engine.EngineFunc(func(_ context.Context, r engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		got = r.Attributes
		return b, nil
	})

	req := newReq("hi")
	req.TaskID = "task-1"
	req.ContextID = "ctx-1"
	req.RunID = "run-1"

	_, err := agent.Run(context.Background(), agent.Agent{ID: "agent-x"}, eng, req,
		agent.WithAttributes(map[string]string{"tenant": "acme", "agent_id": "caller-overrides"}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Caller-supplied keys win on conflict — that's the documented
	// rule on mergeAttributes ("agent never overwrites" = the
	// well-known seed never overwrites caller intent).
	want := map[string]string{
		"agent_id":   "caller-overrides",
		"run_id":     "run-1",
		"task_id":    "task-1",
		"context_id": "ctx-1",
		"tenant":     "acme",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Attributes[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestRun_AttributesMapNotMutated(t *testing.T) {
	extras := map[string]string{"tenant": "acme"}

	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithAttributes(extras),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(extras) != 1 || extras["tenant"] != "acme" {
		t.Errorf("WithAttributes mutated caller's map: %+v", extras)
	}
}

func TestRun_InterruptedDefaultsToDiscarded(t *testing.T) {
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "partial"))
		return b, engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserInput, Detail: "bargeIn"})
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusInterrupted {
		t.Errorf("Status = %q, want interrupted", res.Status)
	}
	if res.Cause != engine.CauseUserInput {
		t.Errorf("Cause = %q, want %q", res.Cause, engine.CauseUserInput)
	}
	if res.Committed {
		t.Error("default disposition should set Committed=false on interrupt")
	}
	if !errdefs.IsInterrupted(res.Err) {
		t.Errorf("Err should satisfy errdefs.IsInterrupted; got %v", res.Err)
	}
	if len(res.Messages) != 1 {
		t.Errorf("partial message should still be exposed; got %d messages", len(res.Messages))
	}
}

// foreignInterrupt only satisfies the errdefs marker. agent should
// classify it as interrupted but skip OnInterrupt because there is no
// engine.InterruptedError to destructure.
type foreignInterrupt struct{}

func (foreignInterrupt) Error() string { return "foreign interrupt" }
func (foreignInterrupt) Interrupted()  {}

func TestRun_ForeignInterruptStillClassifiedButObserverSkipsOnInterrupt(t *testing.T) {
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, foreignInterrupt{}
	})

	rec := &recordingObs{}
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusInterrupted {
		t.Errorf("Status = %q, want interrupted", res.Status)
	}
	if res.Cause != engine.CauseUnknown {
		t.Errorf("foreign interrupt should not synthesise a Cause; got %q", res.Cause)
	}
	if rec.interruptCalls != 0 {
		t.Errorf("OnInterrupt should NOT fire for non-engine.InterruptedError; got %d calls", rec.interruptCalls)
	}
	if rec.endCalls != 1 {
		t.Errorf("OnRunEnd should still fire exactly once; got %d", rec.endCalls)
	}
}

func TestRun_ContextCanceledClassified(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	eng := engine.EngineFunc(func(c context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, c.Err()
	})

	res, err := agent.Run(ctx, agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusCanceled {
		t.Errorf("Status = %q, want canceled", res.Status)
	}
	if res.Committed {
		t.Error("canceled run must not be Committed by default")
	}
}

func TestRun_AbortedClassified(t *testing.T) {
	abort := errdefs.Abortedf("simulated abort")
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, abort
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusAborted {
		t.Errorf("Status = %q, want aborted", res.Status)
	}
	if !errors.Is(res.Err, abort) {
		t.Errorf("Err should preserve the original abort: %v", res.Err)
	}
}

func TestRun_FailedFallsThrough(t *testing.T) {
	plain := errors.New("boom")
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, plain
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusFailed {
		t.Errorf("Status = %q, want failed", res.Status)
	}
	if !errors.Is(res.Err, plain) {
		t.Errorf("Err should wrap original; got %v", res.Err)
	}
}

func TestRun_NewMessagesIsTrailingAssistantBlock(t *testing.T) {
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		// Engine produces three assistant messages in a row.
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "step 1"))
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "step 2"))
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "step 3"))
		return b, nil
	})

	// Pre-seed the board with an assistant message that should NOT be
	// counted as "new" (because it's part of the seeded transcript).
	seeder := agent.BoardSeederFunc(func(_ context.Context, _ agent.RunInfo, req *agent.Request) (*engine.Board, error) {
		b := engine.NewBoard()
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "old answer"))
		b.AppendChannelMessage(engine.MainChannel, req.Message)
		return b, nil
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithBoardSeed(seeder),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := len(res.Messages), 3; got != want {
		t.Errorf("Result.Messages count = %d, want %d (only trailing assistant block)", got, want)
	}
	if res.Messages[0].Content() != "step 1" {
		t.Errorf("first new message = %q, want %q", res.Messages[0].Content(), "step 1")
	}
}

func TestRun_NoNewMessagesWhenLastIsUser(t *testing.T) {
	// Engine returns without producing any assistant message, so the
	// last entry on MainChannel is the user request.
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, nil
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("trailing user-message run should yield no Result.Messages; got %d", len(res.Messages))
	}
}

func TestRun_SeederErrorFailsRun(t *testing.T) {
	bad := agent.BoardSeederFunc(func(_ context.Context, _ agent.RunInfo, _ *agent.Request) (*engine.Board, error) {
		return nil, errors.New("seed boom")
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithBoardSeed(bad),
	)
	if err == nil {
		t.Fatal("expected infrastructure error from failing seeder")
	}
	if res != nil {
		t.Errorf("expected nil result on seeder error; got %+v", res)
	}
}

func TestRun_SeederNilBoardFailsRun(t *testing.T) {
	nilSeeder := agent.BoardSeederFunc(func(_ context.Context, _ agent.RunInfo, _ *agent.Request) (*engine.Board, error) {
		return nil, nil
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithBoardSeed(nilSeeder),
	)
	if err == nil {
		t.Fatal("expected error when seeder returns nil board with nil error")
	}
	if res != nil {
		t.Errorf("expected nil result; got %+v", res)
	}
}

func TestRun_EngineReturnsNilBoardFallsBackToSeeded(t *testing.T) {
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, _ *engine.Board) (*engine.Board, error) {
		return nil, nil
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.LastBoard == nil {
		t.Fatal("Run should fall back to seeded board when engine returns nil")
	}
}

// recordingObs counts callback invocations and orders them.
type recordingObs struct {
	agent.BaseObserver

	mu             sync.Mutex
	startCalls     int
	interruptCalls int
	endCalls       int
	order          []string
	lastIntr       engine.Interrupt
	lastResult     *agent.Result
	lastInfo       agent.RunInfo
}

func (r *recordingObs) OnRunStart(_ context.Context, info agent.RunInfo, _ *agent.Request) {
	r.mu.Lock()
	r.startCalls++
	r.order = append(r.order, "start")
	r.lastInfo = info
	r.mu.Unlock()
}

func (r *recordingObs) OnInterrupt(_ context.Context, _ agent.RunInfo, intr engine.Interrupt) {
	r.mu.Lock()
	r.interruptCalls++
	r.order = append(r.order, "interrupt")
	r.lastIntr = intr
	r.mu.Unlock()
}

func (r *recordingObs) OnRunEnd(_ context.Context, _ agent.RunInfo, res *agent.Result) {
	r.mu.Lock()
	r.endCalls++
	r.order = append(r.order, "end")
	r.lastResult = res
	r.mu.Unlock()
}

func TestRun_ObserverLifecycleOrder_Completed(t *testing.T) {
	rec := &recordingObs{}
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rec.startCalls != 1 || rec.endCalls != 1 || rec.interruptCalls != 0 {
		t.Errorf("call counts: start=%d interrupt=%d end=%d; want 1/0/1",
			rec.startCalls, rec.interruptCalls, rec.endCalls)
	}
	if got, want := strings.Join(rec.order, ","), "start,end"; got != want {
		t.Errorf("call order = %q, want %q", got, want)
	}
	if rec.lastResult != res {
		t.Error("OnRunEnd received a result pointer different from the one returned by Run")
	}
}

func TestRun_ObserverLifecycleOrder_Interrupted(t *testing.T) {
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		return b, engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserCancel, Detail: "stop"})
	})

	rec := &recordingObs{}
	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(rec.order, ","), "start,interrupt,end"; got != want {
		t.Errorf("call order = %q, want %q", got, want)
	}
	if rec.lastIntr.Cause != engine.CauseUserCancel || rec.lastIntr.Detail != "stop" {
		t.Errorf("OnInterrupt received Cause=%q Detail=%q; want user_cancel/stop",
			rec.lastIntr.Cause, rec.lastIntr.Detail)
	}
}

func TestRun_ObserverPanicDoesNotCrash(t *testing.T) {
	panicking := agent.BaseObserver{}
	good := &recordingObs{}

	// Wrap a panicking observer behind a closure-typed observer.
	rec := &panicObs{base: panicking}

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithObserver(rec),
		agent.WithObserver(good),
	)
	if err != nil {
		t.Fatalf("Run failed despite panic recovery: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Errorf("Status = %q, want completed", res.Status)
	}
	if good.startCalls != 1 || good.endCalls != 1 {
		t.Errorf("subsequent observer should still fire; got start=%d end=%d",
			good.startCalls, good.endCalls)
	}
}

type panicObs struct {
	base agent.BaseObserver
}

func (p *panicObs) OnRunStart(context.Context, agent.RunInfo, *agent.Request) { panic("boom") }
func (p *panicObs) OnInterrupt(context.Context, agent.RunInfo, engine.Interrupt) {
	panic("boom")
}
func (p *panicObs) OnRunRevise(context.Context, agent.RunInfo, *agent.Result, int) {
	panic("boom")
}
func (p *panicObs) OnRunEnd(context.Context, agent.RunInfo, *agent.Result) { panic("boom") }

func TestRun_AgentScopedObserversFireBeforeCallScoped(t *testing.T) {
	var hits []string
	var mu sync.Mutex
	mark := func(name string) agent.Observer {
		return &markObs{
			onStart: func() {
				mu.Lock()
				hits = append(hits, name)
				mu.Unlock()
			},
		}
	}

	ag := agent.Agent{
		ID:        "a",
		Observers: []agent.Observer{mark("agent-1"), mark("agent-2")},
	}

	_, err := agent.Run(context.Background(), ag, completedEngine("ok"), newReq("hi"),
		agent.WithObserver(mark("call-1")),
		agent.WithObserver(mark("call-2")),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := []string{"agent-1", "agent-2", "call-1", "call-2"}
	if !equalStrings(hits, want) {
		t.Errorf("observer order = %v, want %v", hits, want)
	}
}

type markObs struct {
	agent.BaseObserver
	onStart func()
}

func (m *markObs) OnRunStart(context.Context, agent.RunInfo, *agent.Request) {
	if m.onStart != nil {
		m.onStart()
	}
}

func TestRun_DeciderDiscardOutput(t *testing.T) {
	dec := deciderFunc(func(_ context.Context, _ agent.RunInfo, _ *agent.Request, _ *agent.Result) (agent.FinalizeDecision, error) {
		return agent.FinalizeDecision{DiscardOutput: true, Reason: "moderation"}, nil
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithDecider(dec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Committed {
		t.Error("DiscardOutput should force Committed=false even on completed status")
	}
	if got := res.State["finalize_reason"]; got != "moderation" {
		t.Errorf("finalize_reason = %v, want %q", got, "moderation")
	}
}

func TestRun_DeciderError_RunReturnsError_ButObserverEndStillFires(t *testing.T) {
	boom := errors.New("decider boom")
	dec := deciderFunc(func(_ context.Context, _ agent.RunInfo, _ *agent.Request, _ *agent.Result) (agent.FinalizeDecision, error) {
		return agent.FinalizeDecision{}, boom
	})

	rec := &recordingObs{}
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithDecider(dec),
		agent.WithObserver(rec),
	)
	if !errors.Is(err, boom) {
		t.Fatalf("Run should surface decider error; got %v", err)
	}
	if res == nil {
		t.Fatal("Run should still return populated Result on decider error")
	}
	if rec.endCalls != 1 {
		t.Errorf("OnRunEnd must still fire on decider error; got %d", rec.endCalls)
	}
}

func TestRun_MultipleDecidersOR(t *testing.T) {
	a := deciderFunc(func(context.Context, agent.RunInfo, *agent.Request, *agent.Result) (agent.FinalizeDecision, error) {
		return agent.FinalizeDecision{Reason: "first"}, nil
	})
	b := deciderFunc(func(context.Context, agent.RunInfo, *agent.Request, *agent.Result) (agent.FinalizeDecision, error) {
		return agent.FinalizeDecision{DiscardOutput: true, Reason: "second"}, nil
	})

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithDecider(a),
		agent.WithDecider(b),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Committed {
		t.Error("any DiscardOutput=true should set Committed=false")
	}
	if got := res.State["finalize_reason"]; got != "first" {
		t.Errorf("first non-empty Reason should win; got %v", got)
	}
}

func TestRun_AgentScopedDecidersFireBeforeCallScoped(t *testing.T) {
	var order []string
	var mu sync.Mutex
	mark := func(name string) agent.Decider {
		return deciderFunc(func(context.Context, agent.RunInfo, *agent.Request, *agent.Result) (agent.FinalizeDecision, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return agent.FinalizeDecision{}, nil
		})
	}

	ag := agent.Agent{
		ID:       "a",
		Deciders: []agent.Decider{mark("agent-1")},
	}

	_, err := agent.Run(context.Background(), ag, completedEngine("ok"), newReq("hi"),
		agent.WithDecider(mark("call-1")),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !equalStrings(order, []string{"agent-1", "call-1"}) {
		t.Errorf("decider order = %v, want [agent-1 call-1]", order)
	}
}

// usageReporterEngine reports a fixed delta then completes. Any
// budget error from the host is propagated so the agent layer sees
// the same termination shape it would observe in a real sandbox host.
func usageReporterEngine(u model.TokenUsage) engine.Engine {
	return engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		if err := h.ReportUsage(ctx, u); err != nil {
			return b, err
		}
		if err := h.ReportUsage(ctx, u); err != nil {
			return b, err
		}
		return b, nil
	})
}

// usageHost is the canonical pattern for callers that want token-usage
// aggregation: embed engine.NoopHost, override ReportUsage. Lives in
// the test file as the worked example for [WithEngineHost] doc.
type usageHost struct {
	engine.NoopHost

	mu    sync.Mutex
	total model.TokenUsage
}

func (h *usageHost) ReportUsage(_ context.Context, u model.TokenUsage) error {
	h.mu.Lock()
	h.total = h.total.Add(u)
	h.mu.Unlock()
	return nil
}

func (h *usageHost) Total() model.TokenUsage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func TestRun_CustomHostAccumulatesUsage(t *testing.T) {
	delta := model.TokenUsage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}
	host := &usageHost{}

	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"},
		usageReporterEngine(delta), newReq("hi"),
		agent.WithEngineHost(host),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := model.TokenUsage{InputTokens: 10, OutputTokens: 14, TotalTokens: 24}
	if got := host.Total(); got != want {
		t.Errorf("Total = %+v, want %+v", got, want)
	}
}

// TestRun_DefaultHostIsNoop pins the documented fallback behaviour.
// Without WithEngineHost the engine's host is engine.NoopHost, so
// ReportUsage / Publish / etc. all silently drop. The run still
// succeeds — just produces no observability.
func TestRun_DefaultHostIsNoop(t *testing.T) {
	delta := model.TokenUsage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}

	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"},
		usageReporterEngine(delta), newReq("hi"),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Errorf("Status = %q, want completed", res.Status)
	}
}

func TestRun_DefaultSeederCopiesInputs(t *testing.T) {
	var got map[string]any
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		got = b.Vars()
		return b, nil
	})

	req := newReq("hi")
	req.Inputs = map[string]any{"k1": "v1", "k2": 42}

	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["k1"] != "v1" || got["k2"] != 42 {
		t.Errorf("default seeder did not copy req.Inputs; vars = %+v", got)
	}
}

func TestRun_RunInfoFieldsPropagated(t *testing.T) {
	rec := &recordingObs{}

	req := newReq("hi")
	req.TaskID = "t-1"
	req.ContextID = "c-1"
	req.RunID = "run-99"

	_, err := agent.Run(context.Background(), agent.Agent{ID: "agent-7"}, completedEngine("ok"), req,
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := agent.RunInfo{AgentID: "agent-7", RunID: "run-99", TaskID: "t-1", ContextID: "c-1"}
	if rec.lastInfo != want {
		t.Errorf("RunInfo = %+v, want %+v", rec.lastInfo, want)
	}
}

func TestRun_NilOptionsAreSkipped(t *testing.T) {
	// nil options must be tolerated for ergonomic call sites.
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		nil,
		agent.WithAttributes(map[string]string{"x": "y"}),
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Errorf("Status = %q, want completed", res.Status)
	}
}

// helper: deciderFunc adapts a closure into agent.Decider.
type deciderFunc func(ctx context.Context, info agent.RunInfo, req *agent.Request, res *agent.Result) (agent.FinalizeDecision, error)

func (f deciderFunc) BeforeFinalize(ctx context.Context, info agent.RunInfo, req *agent.Request, res *agent.Result) (agent.FinalizeDecision, error) {
	return f(ctx, info, req, res)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// counterEngine is a sanity check that EngineFunc adapts atomic-safe
// closures correctly. Not strictly part of the agent contract; here
// for race-detector smoke coverage of the host plumbing.
func counterEngine(counter *int64) engine.Engine {
	return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		atomic.AddInt64(counter, 1)
		return b, nil
	})
}

func TestRun_RaceSmoke(t *testing.T) {
	var counter int64

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, counterEngine(&counter), newReq("x"))
			if err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := atomic.LoadInt64(&counter); got != 16 {
		t.Errorf("expected 16 runs; got %d", got)
	}
}

// TestRun_WithResumeFrom_PropagatesCheckpointAndOverridesRunID
// asserts that the agent threads ResumeFrom into engine.Run and
// rewrites Run.ID to cp.ExecID so the engine's CanResume sees a
// matching id pair (cross-id is the engine's "fork, not resume"
// signal, which the engine surfaces as Validation).
func TestRun_WithResumeFrom_PropagatesCheckpointAndOverridesRunID(t *testing.T) {
	var sawRun engine.Run
	eng := engine.EngineFunc(func(_ context.Context, r engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		sawRun = r
		return b, nil
	})

	cp := &engine.Checkpoint{ExecID: "saved-run-7", Step: "node-x"}

	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng,
		// req.RunID intentionally different so the override path is exercised.
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi"), RunID: "fresh-id"},
		agent.WithResumeFrom(cp),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawRun.ResumeFrom != cp {
		t.Errorf("ResumeFrom = %+v, want pointer to cp", sawRun.ResumeFrom)
	}
	if sawRun.ID != "saved-run-7" {
		t.Errorf("Run.ID = %q, want cp.ExecID %q (resume must override req.RunID)", sawRun.ID, "saved-run-7")
	}
}

// TestRun_WithResumeFrom_NilIsNoop documents that passing a nil
// checkpoint behaves exactly like not passing the option at all —
// fresh start, fresh run id from req.RunID or mintRunID().
func TestRun_WithResumeFrom_NilIsNoop(t *testing.T) {
	var sawRun engine.Run
	eng := engine.EngineFunc(func(_ context.Context, r engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		sawRun = r
		return b, nil
	})
	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi"), RunID: "fresh-id"},
		agent.WithResumeFrom(nil),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sawRun.ResumeFrom != nil {
		t.Errorf("ResumeFrom = %+v, want nil for fresh start", sawRun.ResumeFrom)
	}
	if sawRun.ID != "fresh-id" {
		t.Errorf("Run.ID = %q, want %q (no resume → no override)", sawRun.ID, "fresh-id")
	}
}

// reviseDecider asks for revise on every Decider call until the
// configured number of decisions has been made. Lets tests pin the
// "stop asking after N" boundary independently of WithMaxRevise.
type reviseDecider struct {
	agent.BaseDecider
	mu      sync.Mutex
	calls   int
	stopAt  int // stop asking for revise once calls > stopAt
	reason  string
	discard bool
}

func (d *reviseDecider) BeforeFinalize(_ context.Context, _ agent.RunInfo, _ *agent.Request, _ *agent.Result) (agent.FinalizeDecision, error) {
	d.mu.Lock()
	d.calls++
	calls := d.calls
	d.mu.Unlock()
	dec := agent.FinalizeDecision{Reason: d.reason, DiscardOutput: d.discard}
	if d.stopAt == 0 || calls <= d.stopAt {
		dec.Revise = true
	}
	return dec, nil
}

// reviseObs records every OnRunRevise call so tests can assert the
// next-attempt index sequence and that the prev result is the
// pre-replacement Result (Status / Attempts as of that attempt).
type reviseObs struct {
	agent.BaseObserver
	mu     sync.Mutex
	starts int
	revise []reviseEvent
	end    *agent.Result
}

type reviseEvent struct {
	prevAttempts int
	nextAttempt  int
}

func (r *reviseObs) OnRunStart(context.Context, agent.RunInfo, *agent.Request) {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
}

func (r *reviseObs) OnRunRevise(_ context.Context, _ agent.RunInfo, prev *agent.Result, next int) {
	r.mu.Lock()
	r.revise = append(r.revise, reviseEvent{prevAttempts: prev.Attempts, nextAttempt: next})
	r.mu.Unlock()
}

func (r *reviseObs) OnRunEnd(_ context.Context, _ agent.RunInfo, res *agent.Result) {
	r.mu.Lock()
	r.end = res
	r.mu.Unlock()
}

// TestRun_Revise_DefaultDisabled asserts the safe default: a
// Decider that asks for Revise has its Reason recorded but does
// NOT trigger another engine call when WithMaxRevise was not set.
func TestRun_Revise_DefaultDisabled(t *testing.T) {
	var calls int
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		calls++
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{reason: "needs better citations"}
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithDecider(d),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("engine calls = %d, want 1 (revise disabled by default)", calls)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
	if got := res.State["finalize_reason"]; got != "needs better citations" {
		t.Errorf("finalize_reason = %v, want recorded even when revise dropped", got)
	}
}

// TestRun_Revise_HonoursMaxBudget asserts the loop runs until the
// budget is reached, not until the Decider stops asking. Caps
// runaway loops on always-asking Deciders.
func TestRun_Revise_HonoursMaxBudget(t *testing.T) {
	var calls int
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		calls++
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{} // always asks for revise
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithDecider(d),
		agent.WithMaxRevise(3),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 3 {
		t.Errorf("engine calls = %d, want 3 (budget cap)", calls)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
}

// TestRun_Revise_StopsWhenDeciderSatisfied asserts the loop exits
// early when no Decider asks for revise — Attempts reflects the
// actual count, not the budget.
func TestRun_Revise_StopsWhenDeciderSatisfied(t *testing.T) {
	var calls int
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		calls++
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{stopAt: 2} // asks twice, then satisfied
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithDecider(d),
		agent.WithMaxRevise(5),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 3 {
		t.Errorf("engine calls = %d, want 3 (2 revises then satisfied)", calls)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
}

// TestRun_Revise_ObserverReceivesPrevResultAndNextAttempt asserts
// the OnRunRevise hook fires once per revise transition with the
// pre-replacement Result and the next attempt index.
func TestRun_Revise_ObserverReceivesPrevResultAndNextAttempt(t *testing.T) {
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{}
	obs := &reviseObs{}
	_, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithDecider(d),
		agent.WithObserver(obs),
		agent.WithMaxRevise(3),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := obs.starts; got != 3 {
		t.Errorf("OnRunStart count = %d, want 3", got)
	}
	if len(obs.revise) != 2 {
		t.Fatalf("OnRunRevise count = %d, want 2 (between attempts 1→2 and 2→3)", len(obs.revise))
	}
	wantSeq := []reviseEvent{
		{prevAttempts: 1, nextAttempt: 2},
		{prevAttempts: 2, nextAttempt: 3},
	}
	for i, ev := range obs.revise {
		if ev != wantSeq[i] {
			t.Errorf("OnRunRevise[%d] = %+v, want %+v", i, ev, wantSeq[i])
		}
	}
}

// TestRun_Revise_NotTriggeredOnNonCompleted asserts a flapping engine
// (returning errors) cannot consume the revise budget — Revise only
// fires for completed runs, so transient infrastructure failures
// surface immediately.
func TestRun_Revise_NotTriggeredOnNonCompleted(t *testing.T) {
	var calls int
	eng := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		calls++
		return b, errors.New("engine flap")
	})
	d := &reviseDecider{} // always asks for revise
	res, err := agent.Run(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithDecider(d),
		agent.WithMaxRevise(5),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if calls != 1 {
		t.Errorf("engine calls = %d, want 1 (failed runs do not retry on revise)", calls)
	}
	if res.Status != agent.StatusFailed {
		t.Errorf("Status = %v, want failed", res.Status)
	}
	if res.Attempts != 1 {
		t.Errorf("Attempts = %d, want 1", res.Attempts)
	}
}

// TestRun_PromotesAgentToolsIntoEngineRunDeps is the end-to-end
// regression for contract-audit #1 ("Agent.Tools is silently
// ignored"). After the run-context plumbing is wired (commits
// 1–3 on this branch), agent.Run MUST surface ag.Tools to the
// engine via engine.Run.Deps[depname.ToolAllowedNames] so the
// llmnode policy gate can act on it.
func TestRun_PromotesAgentToolsIntoEngineRunDeps(t *testing.T) {
	var observed *engine.Dependencies
	eng := engine.EngineFunc(func(_ context.Context, run engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		observed = run.Deps
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	ag := agent.Agent{ID: "researcher", Tools: []string{"search", "fetch"}}
	if _, err := agent.Run(context.Background(), ag, eng, newReq("hi")); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if observed == nil {
		t.Fatal("engine.Run.Deps was nil — Agent.Tools was not promoted")
	}
	got, gerr := engine.GetDep[[]string](observed, depname.ToolAllowedNames)
	if gerr != nil {
		t.Fatalf("ToolAllowedNames missing in engine.Run.Deps: %v", gerr)
	}
	want := []string{"search", "fetch"}
	if len(got) != len(want) {
		t.Fatalf("ToolAllowedNames len = %d, want %d (got %v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ToolAllowedNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestRun_AgentToolsDoesNotOverwriteCallerSuppliedToolAllowedNames
// asserts the same "caller-supplied wins" rule that mergeAttributes
// uses for the attribute bag: a power user that overrode the
// allow-list via WithDependencies must see their value reach the
// engine, not the agent's claim.
func TestRun_AgentToolsDoesNotOverwriteCallerSuppliedToolAllowedNames(t *testing.T) {
	var observed *engine.Dependencies
	eng := engine.EngineFunc(func(_ context.Context, run engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		observed = run.Deps
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	deps := engine.NewDependencies()
	deps.Set(depname.ToolAllowedNames, []string{"caller-pin"})

	ag := agent.Agent{ID: "researcher", Tools: []string{"agent-claim"}}
	if _, err := agent.Run(context.Background(), ag, eng, newReq("hi"),
		agent.WithDependencies(deps)); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got, _ := engine.GetDep[[]string](observed, depname.ToolAllowedNames)
	if len(got) != 1 || got[0] != "caller-pin" {
		t.Errorf("ToolAllowedNames = %v, want [caller-pin] (caller-supplied must win)", got)
	}
}
