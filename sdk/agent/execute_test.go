package agent_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/telemetry"
)

// completedEngine appends a single assistant reply and returns nil.
// Used as the "happy path" engine across run tests.
func completedEngine(reply string) agent.Engine {
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, reply))
		return b, nil
	})
}

func newReq(text string) agent.Request {
	return agent.Request{Message: inference.NewTextMessage(inference.RoleUser, text)}
}

func TestRun_NilEngineRejected(t *testing.T) {
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, nil, newReq("hi"))
	if err == nil {
		t.Fatal("expected error for nil engine")
	}
	if res != nil {
		t.Errorf("expected nil result on infrastructure error, got %+v", res)
	}
}

func TestRun_EmptyAgentIDRejected(t *testing.T) {
	res, err := agent.Execute(context.Background(), agent.Agent{}, completedEngine("ok"), newReq("hi"))
	if err == nil {
		t.Fatal("expected error for empty Agent.ID")
	}
	if res != nil {
		t.Errorf("expected nil result on infrastructure error, got %+v", res)
	}
}

func TestRun_CleanCompletion_Defaults(t *testing.T) {
	res, err := agent.Execute(context.Background(),
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
	eng := agent.EngineFunc(func(_ context.Context, r agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		seen = r.RunID
		return b, nil
	})

	req := newReq("hi")
	req.RunID = "run-explicit-42"

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != "run-explicit-42" {
		t.Errorf("Run.RunID = %q, want propagation of req.RunID", seen)
	}
	if res.RunID != "run-explicit-42" {
		t.Errorf("Result.RunID = %q, want propagation of req.RunID", res.RunID)
	}
}

func TestRun_IdentityIsTypedNotAttributed(t *testing.T) {
	var got agent.Run
	eng := agent.EngineFunc(func(_ context.Context, r agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		got = r
		return b, nil
	})

	req := newReq("hi")
	req.TaskID = "task-1"
	req.ContextID = "ctx-1"
	req.RunID = "run-1"

	_, err := agent.Execute(context.Background(), agent.Agent{ID: "agent-x"}, eng, req,
		agent.WithAttributes(map[string]string{"tenant": "acme"}),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Identity dimensions are typed fields on Run.Identity, never
	// keys in the Attributes bag. Attributes only carry caller
	// extras (plus the harness's own agent.attempt marker).
	wantID := agent.Identity{
		AgentID:        "agent-x",
		RunID:          "run-1",
		TaskID:         "task-1",
		ConversationID: "ctx-1",
	}
	if got.Identity != wantID {
		t.Errorf("Identity = %+v, want %+v", got.Identity, wantID)
	}
	for _, key := range []string{
		telemetry.AttrAgentID, telemetry.AttrRunID,
		telemetry.AttrTaskID, telemetry.AttrConversationID,
	} {
		if v, ok := got.Attributes[key]; ok {
			t.Errorf("Attributes[%q] = %q — identity must NOT leak into the attribute bag", key, v)
		}
	}
	if got.Attributes["tenant"] != "acme" {
		t.Errorf("Attributes[tenant] = %q, want acme (caller extras flow through)", got.Attributes["tenant"])
	}
}

func TestRun_AttributesMapNotMutated(t *testing.T) {
	extras := map[string]string{"tenant": "acme"}

	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "partial"))
		return b, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput, Detail: "bargeIn"})
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusInterrupted {
		t.Errorf("Status = %q, want interrupted", res.Status)
	}
	if res.Cause != agent.CauseUserInput {
		t.Errorf("agent.Cause = %q, want %q", res.Cause, agent.CauseUserInput)
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
// agent.InterruptedError to destructure.
type foreignInterrupt struct{}

func (foreignInterrupt) Error() string { return "foreign interrupt" }
func (foreignInterrupt) Interrupted()  {}

func TestRun_ForeignInterruptStillClassifiedButObserverSkipsOnInterrupt(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, foreignInterrupt{}
	})

	rec := &recordingObs{}
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Status != agent.StatusInterrupted {
		t.Errorf("Status = %q, want interrupted", res.Status)
	}
	if res.Cause != agent.CauseUnknown {
		t.Errorf("foreign interrupt should not synthesise a agent.Cause; got %q", res.Cause)
	}
	if rec.interruptCalls != 0 {
		t.Errorf("OnInterrupt should NOT fire for non-agent.InterruptedError; got %d calls", rec.interruptCalls)
	}
	if rec.endCalls != 1 {
		t.Errorf("OnRunEnd should still fire exactly once; got %d", rec.endCalls)
	}
}

func TestRun_ContextCanceledClassified(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	eng := agent.EngineFunc(func(c context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, c.Err()
	})

	res, err := agent.Execute(ctx, agent.Agent{ID: "a"}, eng, newReq("hi"))
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, abort
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, plain
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		// agent.Engine produces three assistant messages in a row.
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "step 1"))
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "step 2"))
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "step 3"))
		return b, nil
	})

	// Pre-seed the board with an assistant message that should NOT be
	// counted as "new" (because it's part of the seeded transcript).
	seeder := agent.PreparerFunc(func(_ context.Context, _ agent.Identity, req *agent.Request, _ *agent.Board) (*agent.Board, error) {
		b := agent.NewBoard()
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "old answer"))
		b.AppendChannelMessage(agent.MainChannel, req.Message)
		return b, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithPreparer(seeder),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := len(res.Messages), 3; got != want {
		t.Errorf("Result.Messages count = %d, want %d (only trailing assistant block)", got, want)
	}
	if res.Messages[0].Content.Text() != "step 1" {
		t.Errorf("first new message = %q, want %q", res.Messages[0].Content.Text(), "step 1")
	}
}

func TestRun_NoNewMessagesWhenLastIsUser(t *testing.T) {
	// agent.Engine returns without producing any assistant message, so the
	// last entry on agent.MainChannel is the user request.
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res.Messages) != 0 {
		t.Errorf("trailing user-message run should yield no Result.Messages; got %d", len(res.Messages))
	}
}

func TestRun_SeederErrorFailsRun(t *testing.T) {
	bad := agent.PreparerFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, _ *agent.Board) (*agent.Board, error) {
		return nil, errors.New("seed boom")
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithPreparer(bad),
	)
	if err == nil {
		t.Fatal("expected infrastructure error from failing seeder")
	}
	if res != nil {
		t.Errorf("expected nil result on seeder error; got %+v", res)
	}
}

func TestRun_SeederNilBoardFailsRun(t *testing.T) {
	nilSeeder := agent.PreparerFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, _ *agent.Board) (*agent.Board, error) {
		return nil, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithPreparer(nilSeeder),
	)
	if err == nil {
		t.Fatal("expected error when seeder returns nil board with nil error")
	}
	if res != nil {
		t.Errorf("expected nil result; got %+v", res)
	}
}

func TestRun_EngineReturnsNilBoardFallsBackToSeeded(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, _ *agent.Board) (*agent.Board, error) {
		return nil, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.LastBoard == nil {
		t.Fatal("agent.Run should fall back to seeded board when engine returns nil")
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
	lastIntr       agent.Interrupt
	lastResult     *agent.Result
	lastInfo       agent.Identity
}

func (r *recordingObs) OnRunStart(_ context.Context, info agent.Identity, _ *agent.Request) {
	r.mu.Lock()
	r.startCalls++
	r.order = append(r.order, "start")
	r.lastInfo = info
	r.mu.Unlock()
}

func (r *recordingObs) OnInterrupt(_ context.Context, _ agent.Identity, intr agent.Interrupt) {
	r.mu.Lock()
	r.interruptCalls++
	r.order = append(r.order, "interrupt")
	r.lastIntr = intr
	r.mu.Unlock()
}

func (r *recordingObs) OnRunEnd(_ context.Context, _ agent.Identity, res *agent.Result) {
	r.mu.Lock()
	r.endCalls++
	r.order = append(r.order, "end")
	r.lastResult = res
	r.mu.Unlock()
}

func TestRun_ObserverLifecycleOrder_Completed(t *testing.T) {
	rec := &recordingObs{}
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
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
		t.Error("OnRunEnd received a result pointer different from the one returned by agent.Run")
	}
}

func TestRun_ObserverLifecycleOrder_Interrupted(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput, Detail: "stop"})
	})

	rec := &recordingObs{}
	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got, want := strings.Join(rec.order, ","), "start,interrupt,end"; got != want {
		t.Errorf("call order = %q, want %q", got, want)
	}
	if rec.lastIntr.Cause != agent.CauseUserInput || rec.lastIntr.Detail != "stop" {
		t.Errorf("OnInterrupt received agent.Cause=%q Detail=%q; want user_cancel/stop",
			rec.lastIntr.Cause, rec.lastIntr.Detail)
	}
}

func TestRun_ObserverPanicDoesNotCrash(t *testing.T) {
	panicking := agent.BaseObserver{}
	good := &recordingObs{}

	// Wrap a panicking observer behind a closure-typed observer.
	rec := &panicObs{base: panicking}

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithObserver(rec),
		agent.WithObserver(good),
	)
	if err != nil {
		t.Fatalf("agent.Run failed despite panic recovery: %v", err)
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

func (p *panicObs) OnRunStart(context.Context, agent.Identity, *agent.Request) { panic("boom") }
func (p *panicObs) OnInterrupt(context.Context, agent.Identity, agent.Interrupt) {
	panic("boom")
}
func (p *panicObs) OnRunRevise(context.Context, agent.Identity, *agent.Result, int) {
	panic("boom")
}
func (p *panicObs) OnRunEnd(context.Context, agent.Identity, *agent.Result) { panic("boom") }

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

	_, err := agent.Execute(context.Background(), ag, completedEngine("ok"), newReq("hi"),
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

func (m *markObs) OnRunStart(context.Context, agent.Identity, *agent.Request) {
	if m.onStart != nil {
		m.onStart()
	}
}

type endObserver struct {
	agent.BaseObserver
	onEnd func(*agent.Result)
}

func (o *endObserver) OnRunEnd(_ context.Context, _ agent.Identity, res *agent.Result) {
	if o.onEnd != nil {
		o.onEnd(res)
	}
}

func TestRun_CommitterRunsAfterRefereeBeforeObserver(t *testing.T) {
	var order []string
	referee := deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
		order = append(order, "referee")
		return agent.Decision{}, nil
	})
	committer := agent.CommitterFunc(func(_ context.Context, id agent.Identity, _ *agent.Request, res *agent.Result) error {
		if id.RunID == "" {
			t.Error("Committer received empty RunID")
		}
		if !res.Committed {
			t.Error("Committer received uncommitted result")
		}
		order = append(order, "committer")
		return nil
	})
	observer := &endObserver{onEnd: func(*agent.Result) {
		order = append(order, "observer")
	}}

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithObserver(observer),
		agent.WithCommitter(committer),
		agent.WithReferee(referee),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !res.Committed {
		t.Fatal("successful Committer should preserve Committed=true")
	}
	if got, want := strings.Join(order, ","), "referee,committer,observer"; got != want {
		t.Fatalf("lifecycle order = %q, want %q", got, want)
	}
}

func TestRun_CommitterFailureReturnsResultAndNotifiesObserver(t *testing.T) {
	boom := errors.New("commit boom")
	var observed *agent.Result
	observer := &endObserver{onEnd: func(res *agent.Result) {
		observed = res
	}}

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
			return boom
		})),
		agent.WithObserver(observer),
	)
	if !errors.Is(err, boom) {
		t.Fatalf("Execute error = %v, want wrapped Committer error", err)
	}
	if res == nil {
		t.Fatal("Execute returned nil Result after Committer failure")
	}
	if res.Committed {
		t.Fatal("Committer failure should set Committed=false")
	}
	if got := res.State["finalize_reason"]; got != "commit_failed" {
		t.Fatalf("finalize_reason = %v, want commit_failed", got)
	}
	if observed != res {
		t.Fatal("Observer did not receive the failed-commit Result")
	}
	if observed.Committed {
		t.Fatal("Observer saw Committed=true after Committer failure")
	}
}

func TestRun_CommitterSkippedForUncommittedResult(t *testing.T) {
	tests := map[string]struct {
		engine  agent.Engine
		referee agent.Referee
	}{
		"discarded": {
			engine: completedEngine("ok"),
			referee: deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
				return agent.Decision{DiscardOutput: true}, nil
			}),
		},
		"failed": {
			engine: agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
				return b, errors.New("engine failed")
			}),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			calls := 0
			opts := []agent.ExecuteOption{
				agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
					calls++
					return nil
				})),
			}
			if tc.referee != nil {
				opts = append(opts, agent.WithReferee(tc.referee))
			}
			res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, tc.engine, newReq("hi"), opts...)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if res.Committed {
				t.Fatal("test setup produced committed Result")
			}
			if calls != 0 {
				t.Fatalf("Committer calls = %d, want 0", calls)
			}
		})
	}
}

func TestRun_CommitterRunsOnceAfterFinalReviseAttempt(t *testing.T) {
	refereeCalls := 0
	committerCalls := 0
	referee := deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
		refereeCalls++
		return agent.Decision{Revise: refereeCalls == 1}, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithReferee(referee),
		agent.WithMaxRevise(2),
		agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
			committerCalls++
			return nil
		})),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", res.Attempts)
	}
	if committerCalls != 1 {
		t.Fatalf("Committer calls = %d, want 1", committerCalls)
	}
}

func TestRun_AgentScopedCommittersRunBeforeCallScoped(t *testing.T) {
	var order []string
	mark := func(name string) agent.Committer {
		return agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
			order = append(order, name)
			return nil
		})
	}
	ag := agent.Agent{
		ID:         "a",
		Committers: []agent.Committer{mark("agent-1"), mark("agent-2")},
	}

	_, err := agent.Execute(context.Background(), ag, completedEngine("ok"), newReq("hi"),
		agent.WithCommitter(mark("call-1")),
		agent.WithCommitter(mark("call-2")),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got, want := strings.Join(order, ","), "agent-1,agent-2,call-1,call-2"; got != want {
		t.Fatalf("Committer order = %q, want %q", got, want)
	}
}

func TestRun_DeciderDiscardOutput(t *testing.T) {
	dec := deciderFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, _ *agent.Result) (agent.Decision, error) {
		return agent.Decision{DiscardOutput: true, Reason: "moderation"}, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithReferee(dec),
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
	dec := deciderFunc(func(_ context.Context, _ agent.Identity, _ *agent.Request, _ *agent.Result) (agent.Decision, error) {
		return agent.Decision{}, boom
	})

	rec := &recordingObs{}
	commitCalls := 0
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithReferee(dec),
		agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
			commitCalls++
			return nil
		})),
		agent.WithObserver(rec),
	)
	if !errors.Is(err, boom) {
		t.Fatalf("agent.Run should surface decider error; got %v", err)
	}
	if res == nil {
		t.Fatal("agent.Run should still return populated Result on decider error")
	}
	if rec.endCalls != 1 {
		t.Errorf("OnRunEnd must still fire on decider error; got %d", rec.endCalls)
	}
	if res.Committed {
		t.Error("Referee error should leave Result uncommitted")
	}
	if got := res.State["finalize_reason"]; got != "referee_failed" {
		t.Errorf("finalize_reason = %v, want referee_failed", got)
	}
	if commitCalls != 0 {
		t.Errorf("Committer calls = %d, want 0 after Referee error", commitCalls)
	}
}

func TestRun_MultipleDecidersOR(t *testing.T) {
	a := deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
		return agent.Decision{Reason: "first"}, nil
	})
	b := deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
		return agent.Decision{DiscardOutput: true, Reason: "second"}, nil
	})

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
		agent.WithReferee(a),
		agent.WithReferee(b),
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
	mark := func(name string) agent.Referee {
		return deciderFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) (agent.Decision, error) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return agent.Decision{}, nil
		})
	}

	ag := agent.Agent{
		ID:       "a",
		Referees: []agent.Referee{mark("agent-1")},
	}

	_, err := agent.Execute(context.Background(), ag, completedEngine("ok"), newReq("hi"),
		agent.WithReferee(mark("call-1")),
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
func usageReporterEngine(u inference.Usage) agent.Engine {
	return agent.EngineFunc(func(ctx context.Context, _ agent.Run, h agent.Host, b *agent.Board) (*agent.Board, error) {
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
// aggregation: embed agent.NoopHost, override ReportUsage. Lives in
// the test file as the worked example for [WithHost] doc.
type usageHost struct {
	agent.NoopHost

	mu    sync.Mutex
	total inference.Usage
}

func (h *usageHost) ReportUsage(_ context.Context, u inference.Usage) error {
	h.mu.Lock()
	h.total = h.total.Add(u)
	h.mu.Unlock()
	return nil
}

func (h *usageHost) Total() inference.Usage {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.total
}

func TestRun_CustomHostAccumulatesUsage(t *testing.T) {
	delta := inference.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}
	host := &usageHost{}

	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"},
		usageReporterEngine(delta), newReq("hi"),
		agent.WithHost(host),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := inference.Usage{InputTokens: 10, OutputTokens: 14, TotalTokens: 24}
	got := host.Total()
	if got.InputTokens != want.InputTokens ||
		got.OutputTokens != want.OutputTokens ||
		got.TotalTokens != want.TotalTokens {
		t.Errorf("Total = %+v, want token totals %+v", got, want)
	}
}

// TestRun_DefaultHostIsNoop pins the documented fallback behaviour.
// Without WithHost the engine's host is agent.NoopHost, so
// ReportUsage / Publish / etc. all silently drop. The run still
// succeeds — just produces no observability.
func TestRun_DefaultHostIsNoop(t *testing.T) {
	delta := inference.Usage{InputTokens: 5, OutputTokens: 7, TotalTokens: 12}

	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"},
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		got = b.Vars()
		return b, nil
	})

	req := newReq("hi")
	req.Inputs = map[string]any{"k1": "v1", "k2": 42}

	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, req)
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

	_, err := agent.Execute(context.Background(), agent.Agent{ID: "agent-7"}, completedEngine("ok"), req,
		agent.WithObserver(rec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := agent.Identity{AgentID: "agent-7", RunID: "run-99", TaskID: "t-1", ConversationID: "c-1"}
	if rec.lastInfo != want {
		t.Errorf("Identity = %+v, want %+v", rec.lastInfo, want)
	}
}

func TestRun_NilOptionsAreSkipped(t *testing.T) {
	// nil options must be tolerated for ergonomic call sites.
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, completedEngine("ok"), newReq("hi"),
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

// helper: deciderFunc adapts a closure into agent.Referee.
type deciderFunc func(ctx context.Context, info agent.Identity, req *agent.Request, res *agent.Result) (agent.Decision, error)

func (f deciderFunc) After(ctx context.Context, info agent.Identity, req *agent.Request, res *agent.Result) (agent.Decision, error) {
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

// counterEngine is a sanity check that agent.EngineFunc adapts atomic-safe
// closures correctly. Not strictly part of the agent contract; here
// for race-detector smoke coverage of the host plumbing.
func counterEngine(counter *int64) agent.Engine {
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
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
			_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, counterEngine(&counter), newReq("x"))
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
// asserts that the agent threads ResumeFrom into agent.Run and
// rewrites Run.RunID to cp.ExecID so the engine's CanResume sees a
// matching id pair (cross-id is the engine's "fork, not resume"
// signal, which the engine surfaces as Validation).
func TestRun_WithResumeFrom_PropagatesCheckpointAndOverridesRunID(t *testing.T) {
	var sawRun agent.Run
	eng := agent.EngineFunc(func(_ context.Context, r agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		sawRun = r
		return b, nil
	})

	cp := &agent.Checkpoint{ExecID: "saved-run-7", Steps: []string{"node-x"}}

	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng,
		// req.RunID intentionally different so the override path is exercised.
		agent.Request{Message: inference.NewTextMessage(inference.RoleUser, "hi"), RunID: "fresh-id"},
		agent.WithResumeFrom(cp),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if sawRun.ResumeFrom != cp {
		t.Errorf("ResumeFrom = %+v, want pointer to cp", sawRun.ResumeFrom)
	}
	if sawRun.RunID != "saved-run-7" {
		t.Errorf("Run.RunID = %q, want cp.ExecID %q (resume must override req.RunID)", sawRun.RunID, "saved-run-7")
	}
}

// TestRun_WithResumeFrom_NilIsNoop documents that passing a nil
// checkpoint behaves exactly like not passing the option at all —
// fresh start, fresh run id from req.RunID or mintRunID().
func TestRun_WithResumeFrom_NilIsNoop(t *testing.T) {
	var sawRun agent.Run
	eng := agent.EngineFunc(func(_ context.Context, r agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		sawRun = r
		return b, nil
	})
	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng,
		agent.Request{Message: inference.NewTextMessage(inference.RoleUser, "hi"), RunID: "fresh-id"},
		agent.WithResumeFrom(nil),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if sawRun.ResumeFrom != nil {
		t.Errorf("ResumeFrom = %+v, want nil for fresh start", sawRun.ResumeFrom)
	}
	if sawRun.RunID != "fresh-id" {
		t.Errorf("Run.RunID = %q, want %q (no resume → no override)", sawRun.RunID, "fresh-id")
	}
}

// reviseDecider asks for revise on every Referee call until the
// configured number of decisions has been made. Lets tests pin the
// "stop asking after N" boundary independently of WithMaxRevise.
type reviseDecider struct {
	agent.BaseReferee
	mu      sync.Mutex
	calls   int
	stopAt  int // stop asking for revise once calls > stopAt
	reason  string
	discard bool
}

func (d *reviseDecider) After(_ context.Context, _ agent.Identity, _ *agent.Request, _ *agent.Result) (agent.Decision, error) {
	d.mu.Lock()
	d.calls++
	calls := d.calls
	d.mu.Unlock()
	dec := agent.Decision{Reason: d.reason, DiscardOutput: d.discard}
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

func (r *reviseObs) OnRunStart(context.Context, agent.Identity, *agent.Request) {
	r.mu.Lock()
	r.starts++
	r.mu.Unlock()
}

func (r *reviseObs) OnRunRevise(_ context.Context, _ agent.Identity, prev *agent.Result, next int) {
	r.mu.Lock()
	r.revise = append(r.revise, reviseEvent{prevAttempts: prev.Attempts, nextAttempt: next})
	r.mu.Unlock()
}

func (r *reviseObs) OnRunEnd(_ context.Context, _ agent.Identity, res *agent.Result) {
	r.mu.Lock()
	r.end = res
	r.mu.Unlock()
}

// TestRun_Revise_DefaultDisabled asserts the safe default: a
// Referee that asks for Revise has its Reason recorded but does
// NOT trigger another engine call when WithMaxRevise was not set.
func TestRun_Revise_DefaultDisabled(t *testing.T) {
	var calls int
	var commitCalls int
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		calls++
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{reason: "needs better citations"}
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithReferee(d),
		agent.WithCommitter(agent.CommitterFunc(func(context.Context, agent.Identity, *agent.Request, *agent.Result) error {
			commitCalls++
			return nil
		})),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
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
	if !res.Committed || commitCalls != 1 {
		t.Errorf("unhonored Revise should remain committable: Committed=%v calls=%d", res.Committed, commitCalls)
	}
}

// TestRun_Revise_HonoursMaxBudget asserts the loop runs until the
// budget is reached, not until the Referee stops asking. Caps
// runaway loops on always-asking After.
func TestRun_Revise_HonoursMaxBudget(t *testing.T) {
	var calls int
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		calls++
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{} // always asks for revise
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithReferee(d),
		agent.WithMaxRevise(3),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if calls != 3 {
		t.Errorf("engine calls = %d, want 3 (budget cap)", calls)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
}

// TestRun_Revise_StopsWhenDeciderSatisfied asserts the loop exits
// early when no Referee asks for revise — Attempts reflects the
// actual count, not the budget.
func TestRun_Revise_StopsWhenDeciderSatisfied(t *testing.T) {
	var calls int
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		calls++
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{stopAt: 2} // asks twice, then satisfied
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithReferee(d),
		agent.WithMaxRevise(5),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})
	d := &reviseDecider{}
	obs := &reviseObs{}
	_, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithReferee(d),
		agent.WithObserver(obs),
		agent.WithMaxRevise(3),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
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
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		calls++
		return b, errors.New("engine flap")
	})
	d := &reviseDecider{} // always asks for revise
	res, err := agent.Execute(context.Background(), agent.Agent{ID: "a"}, eng, newReq("hi"),
		agent.WithReferee(d),
		agent.WithMaxRevise(5),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
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

// TestRun_PromotesAgentToolsIntoToolAllowList is the end-to-end
// regression for contract-audit #1 ("Agent.Tools is silently
// ignored"): agent.Execute MUST surface ag.Tools to the engine
// via Run.ToolAllowList so the engine's policy gate can act on it.
func TestRun_PromotesAgentToolsIntoToolAllowList(t *testing.T) {
	var observed []string
	eng := agent.EngineFunc(func(_ context.Context, run agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		observed = run.ToolAllowList
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	ag := agent.Agent{ID: "researcher", Tools: []string{"search", "fetch"}}
	if _, err := agent.Execute(context.Background(), ag, eng, newReq("hi")); err != nil {
		t.Fatalf("agent.Execute: %v", err)
	}

	want := []string{"search", "fetch"}
	if len(observed) != len(want) {
		t.Fatalf("ToolAllowList = %v, want %v", observed, want)
	}
	for i := range want {
		if observed[i] != want[i] {
			t.Errorf("ToolAllowList[%d] = %q, want %q", i, observed[i], want[i])
		}
	}
}

// TestRun_ToolAllowListCallerSuppliedWins asserts the same
// "caller-supplied wins" rule WithAttributes uses for the attribute
// bag: a power user that overrode the allow-list via
// WithToolAllowList must see their value reach the engine, not the
// agent's claim.
func TestRun_ToolAllowListCallerSuppliedWins(t *testing.T) {
	var observed []string
	eng := agent.EngineFunc(func(_ context.Context, run agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		observed = run.ToolAllowList
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	ag := agent.Agent{ID: "researcher", Tools: []string{"agent-claim"}}
	if _, err := agent.Execute(context.Background(), ag, eng, newReq("hi"),
		agent.WithToolAllowList([]string{"caller-pin"})); err != nil {
		t.Fatalf("agent.Execute: %v", err)
	}

	if len(observed) != 1 || observed[0] != "caller-pin" {
		t.Errorf("ToolAllowList = %v, want [caller-pin] (caller-supplied must win)", observed)
	}
}

// TestRun_ToolAllowListIsDefensiveCopy pins the "stable snapshot"
// contract: mutating Agent.Tools after Execute returns must not
// leak into the slice the engine observed during the run.
func TestRun_ToolAllowListIsDefensiveCopy(t *testing.T) {
	observed := make(chan []string, 1)
	eng := agent.EngineFunc(func(_ context.Context, run agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		cp := append([]string(nil), run.ToolAllowList...)
		run.ToolAllowList[0] = "engine-mutated"
		observed <- cp
		return b, nil
	})

	ag := agent.Agent{ID: "researcher", Tools: []string{"search"}}
	if _, err := agent.Execute(context.Background(), ag, eng, newReq("hi")); err != nil {
		t.Fatalf("agent.Execute: %v", err)
	}
	ag.Tools[0] = "caller-mutated"

	got := <-observed
	if len(got) != 1 || got[0] != "search" {
		t.Errorf("snapshot = %v, want [search] (neither engine nor caller mutation may leak)", got)
	}
}

// TestRun_WithParentRunID_PropagatesToEngineRun is the regression
// test for contract-audit #3. Run.ParentRunID was a typed
// field with zero writers before this PR; agent.Run now promotes
// the WithParentRunID value into every dispatched agent.Run so
// the multi-agent call chain finally has a stable correlation
// dimension dashboards / pod controllers can rely on.
func TestRun_WithParentRunID_PropagatesToEngineRun(t *testing.T) {
	var observed string
	eng := agent.EngineFunc(func(_ context.Context, run agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		observed = run.ParentRunID
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	if _, err := agent.Execute(context.Background(),
		agent.Agent{ID: "child"}, eng, newReq("hi"),
		agent.WithParentRunID("run-parent-42"),
	); err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if observed != "run-parent-42" {
		t.Fatalf("Run.ParentRunID = %q, want %q", observed, "run-parent-42")
	}
}

// TestRun_WithParentRunID_EmptyIsNoop documents the no-op contract:
// callers (host applications, future pod controllers) that don't
// have a parent id MUST be able to omit the option without seeing
// an "empty parent" appear downstream.
func TestRun_WithParentRunID_EmptyIsNoop(t *testing.T) {
	var observed string
	var sawHookCall bool
	eng := agent.EngineFunc(func(_ context.Context, run agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		sawHookCall = true
		observed = run.ParentRunID
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	if _, err := agent.Execute(context.Background(),
		agent.Agent{ID: "x"}, eng, newReq("hi")); err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if !sawHookCall {
		t.Fatal("engine never called")
	}
	if observed != "" {
		t.Fatalf("ParentRunID should default to empty string, got %q", observed)
	}
}

// TestRun_WithArtifactChannels_HarvestsRegisteredChannels is the
// regression for contract-audit #6. Result.Artifacts had been
// promised since v0.1 ("engines store them in a board channel;
// agent collects channel contents into Artifacts on the way
// out") but no agent.Run code path implemented the harvest, so
// the field was permanently nil for every caller.
func TestRun_WithArtifactChannels_HarvestsRegisteredChannels(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		// agent.Engine writes artifacts onto two dedicated channels.
		b.AppendChannelMessage("summary",
			inference.Message{Role: inference.RoleAssistant, Content: inference.Content{Parts: []inference.Part{
				inference.TextPart{Text: "tl;dr line 1"},
			}}})
		b.AppendChannelMessage("summary",
			inference.Message{Role: inference.RoleAssistant, Content: inference.Content{Parts: []inference.Part{
				inference.TextPart{Text: "tl;dr line 2"},
			}}})
		b.AppendChannelMessage("audio",
			inference.Message{Role: inference.RoleAssistant, Content: inference.Content{Parts: []inference.Part{
				inference.TextPart{Text: "<wav-blob>"},
			}}})
		// agent.MainChannel reply still happens (Result.Messages path).
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "x"}, eng, newReq("hi"),
		agent.WithArtifactChannels("summary", "audio"),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}

	if len(res.Artifacts) != 2 {
		t.Fatalf("len(Artifacts) = %d, want 2 (got %+v)", len(res.Artifacts), res.Artifacts)
	}
	if res.Artifacts[0].Name != "summary" || res.Artifacts[1].Name != "audio" {
		t.Errorf("artifacts not in registration order: got %q / %q",
			res.Artifacts[0].Name, res.Artifacts[1].Name)
	}
	if len(res.Artifacts[0].Parts) != 2 {
		t.Errorf("summary.Parts len = %d, want 2 (one Part per Message)",
			len(res.Artifacts[0].Parts))
	}
	textAt := func(i int) string {
		tp, ok := res.Artifacts[0].Parts[i].(inference.TextPart)
		if !ok {
			t.Fatalf("summary part %d is %T, want inference.TextPart", i, res.Artifacts[0].Parts[i])
		}
		return tp.Text
	}
	if textAt(0) != "tl;dr line 1" || textAt(1) != "tl;dr line 2" {
		t.Errorf("summary parts not in board-write order: %+v", res.Artifacts[0].Parts)
	}
}

// TestRun_WithArtifactChannels_DefaultIsNilArtifacts asserts the
// back-compat invariant: callers that don't opt in must keep
// seeing nil Artifacts. Without this guard a future "auto-harvest
// every non-Main channel" refactor could surface unrelated
// internal channels and break consumers that switch on
// len(res.Artifacts) == 0.
func TestRun_WithArtifactChannels_DefaultIsNilArtifacts(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage("internal",
			inference.NewTextMessage(inference.RoleAssistant, "noise"))
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})
	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "x"}, eng, newReq("hi"))
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Artifacts != nil {
		t.Errorf("Artifacts = %+v, want nil (no opt-in)", res.Artifacts)
	}
}

// TestRun_WithArtifactChannels_EmptyChannelDropped documents the
// "no empty bundles" rule: channels that exist but hold zero
// messages (or zero Parts across all messages) are silently
// skipped so consumers can rely on `for _, a := range Artifacts`
// always yielding renderable payload.
func TestRun_WithArtifactChannels_EmptyChannelDropped(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage("populated",
			inference.NewTextMessage(inference.RoleAssistant, "hello"))
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "x"}, eng, newReq("hi"),
		agent.WithArtifactChannels("populated", "missing-channel"),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].Name != "populated" {
		t.Fatalf("missing-channel should have been dropped, got %+v", res.Artifacts)
	}
}

// TestRun_WithArtifactChannels_MainChannelSilentlySkipped documents
// the explicit "agent.MainChannel is not an artifact channel" rule. agent.Run
// already promotes agent.MainChannel into Result.Messages; harvesting it
// again would duplicate the same payload across two semantically
// different fields (Messages keeps role + tool metadata; Artifacts
// is the modality-bundle view).
func TestRun_WithArtifactChannels_MainChannelSilentlySkipped(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "would-duplicate"))
		return b, nil
	})

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "x"}, eng, newReq("hi"),
		agent.WithArtifactChannels(agent.MainChannel, ""),
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if res.Artifacts != nil {
		t.Errorf("agent.MainChannel + empty must be filtered out, got %+v", res.Artifacts)
	}
}

// TestRun_WithArtifactChannels_AccumulatesAndDedupes confirms
// per-agent / per-call composition: multiple WithArtifactChannels
// calls accumulate so an Agent can declare the standard channels
// once and individual callers can extend the list.
func TestRun_WithArtifactChannels_AccumulatesAndDedupes(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage("a", inference.NewTextMessage(inference.RoleAssistant, "a-msg"))
		b.AppendChannelMessage("b", inference.NewTextMessage(inference.RoleAssistant, "b-msg"))
		b.AppendChannelMessage(agent.MainChannel,
			inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	})

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "x"}, eng, newReq("hi"),
		agent.WithArtifactChannels("a"),
		agent.WithArtifactChannels("a", "b"), // dup "a" must not double-emit
	)
	if err != nil {
		t.Fatalf("agent.Run: %v", err)
	}
	if len(res.Artifacts) != 2 {
		t.Fatalf("dedup failed: got %d artifacts (%+v)", len(res.Artifacts), res.Artifacts)
	}
	if res.Artifacts[0].Name != "a" || res.Artifacts[1].Name != "b" {
		t.Errorf("registration-order broken: %+v", res.Artifacts)
	}
}

// ---------- Run descriptor ----------

func TestRun_AttributeReturnsValue(t *testing.T) {
	r := agent.Run{Attributes: map[string]string{"tenant": "acme"}}
	if got := r.Attribute("tenant"); got != "acme" {
		t.Errorf("Attribute(tenant) = %q, want acme", got)
	}
}

func TestRun_AttributeMissingReturnsEmpty(t *testing.T) {
	r := agent.Run{Attributes: map[string]string{"tenant": "acme"}}
	if got := r.Attribute("missing"); got != "" {
		t.Errorf("Attribute(missing) = %q, want empty", got)
	}
}

func TestRun_AttributeNilMapSafe(t *testing.T) {
	r := agent.Run{}
	if got := r.Attribute("any"); got != "" {
		t.Errorf("Attribute on nil map = %q, want empty", got)
	}
}

func TestRun_ZeroValueUsable(t *testing.T) {
	// agent.Run is documented as a plain struct; the zero value
	// must compose correctly with EngineFunc dispatch.
	r := agent.Run{}
	if r.RunID != "" {
		t.Errorf("zero ID = %q, want empty", r.RunID)
	}
	if r.ToolAllowList != nil {
		t.Error("zero ToolAllowList must be nil")
	}
	if r.ResumeFrom != nil {
		t.Error("zero ResumeFrom must be nil")
	}
}

// ---------- Resume ----------
// stubEngine records each Execute call so tests can assert that
// ResumeFrom / ResumeContext arrive populated.
type stubEngine struct {
	gotRun     agent.Run
	gotResume  agent.ResumeContext
	gotResOK   bool
	resumeErr  error
	canResume  func(agent.Checkpoint) error
	executions int
}

func (s *stubEngine) Execute(ctx context.Context, run agent.Run, _ agent.Host, board *agent.Board) (*agent.Board, error) {
	s.executions++
	s.gotRun = run
	s.gotResume, s.gotResOK = agent.ResumeContextFromContext(ctx)
	return board, s.resumeErr
}

// resumerEngine bolts CanResume onto stubEngine to cover the optional
// Resumer interface.
type resumerEngine struct {
	stubEngine
}

func (r *resumerEngine) CanResume(cp agent.Checkpoint) error {
	if r.canResume != nil {
		return r.canResume(cp)
	}
	return nil
}

// memStore is a minimal in-memory CheckpointStore for tests.
type memStore struct {
	cp *agent.Checkpoint
}

func (m *memStore) Save(_ context.Context, cp agent.Checkpoint) error {
	cp2 := cp
	m.cp = &cp2
	return nil
}

func (m *memStore) Load(_ context.Context, _ string) (*agent.Checkpoint, error) {
	return m.cp, nil
}

func TestIsResumable(t *testing.T) {
	if agent.IsResumable(&stubEngine{}) {
		t.Fatal("stubEngine must not satisfy Resumer")
	}
	if !agent.IsResumable(&resumerEngine{}) {
		t.Fatal("resumerEngine must satisfy Resumer")
	}
}

func TestLoadAndResume_FreshStart(t *testing.T) {
	eng := &stubEngine{}
	host := agent.NoopHost{}
	store := &memStore{}

	_, err := agent.LoadAndResume(context.Background(), eng, host, store,
		agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil)
	if err != nil {
		t.Fatalf("LoadAndResume: %v", err)
	}
	if eng.executions != 1 {
		t.Fatalf("want 1 execution, got %d", eng.executions)
	}
	if eng.gotRun.ResumeFrom != nil {
		t.Fatal("fresh start must leave Run.ResumeFrom nil")
	}
	if !eng.gotResOK {
		t.Fatal("ResumeContext should be populated even on fresh starts")
	}
	if eng.gotResume.Attempt != 1 {
		t.Fatalf("Attempt = %d, want 1", eng.gotResume.Attempt)
	}
	if eng.gotResume.Signal != "manual" {
		t.Fatalf("default Signal = %q, want manual", eng.gotResume.Signal)
	}
}

func TestLoadAndResume_RequiresCheckpointWhenFreshDisallowed(t *testing.T) {
	eng := &stubEngine{}
	store := &memStore{}

	_, err := agent.LoadAndResume(context.Background(), eng, agent.NoopHost{}, store,
		agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil,
		agent.WithFreshStartAllowed(false),
	)
	if err == nil {
		t.Fatal("expected NotFound error")
	}
	if !errdefs.IsNotFound(err) {
		t.Fatalf("expected NotFound, got %v", err)
	}
	if eng.executions != 0 {
		t.Fatal("must not call Execute when no checkpoint and fresh disallowed")
	}
}

func TestLoadAndResume_ResumePathPopulatesContext(t *testing.T) {
	eng := &stubEngine{}
	cpAt := time.Now().Add(-time.Hour)
	store := &memStore{cp: &agent.Checkpoint{
		ExecID:    "r1",
		Steps:     []string{"node-3"},
		Timestamp: cpAt,
	}}

	_, err := agent.LoadAndResume(context.Background(), eng, agent.NoopHost{}, store,
		agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil,
		agent.WithResumeSignal("crash"),
	)
	if err != nil {
		t.Fatalf("LoadAndResume: %v", err)
	}
	if eng.gotRun.ResumeFrom == nil || len(eng.gotRun.ResumeFrom.Steps) != 1 || eng.gotRun.ResumeFrom.Steps[0] != "node-3" {
		t.Fatalf("ResumeFrom not propagated: %+v", eng.gotRun.ResumeFrom)
	}
	if !eng.gotResOK || eng.gotResume.Attempt < 2 {
		t.Fatalf("resume Attempt should be >= 2, got %d (ok=%v)", eng.gotResume.Attempt, eng.gotResOK)
	}
	if eng.gotResume.Signal != "crash" {
		t.Fatalf("Signal = %q, want crash", eng.gotResume.Signal)
	}
	if !eng.gotResume.CheckpointAt.Equal(cpAt) {
		t.Fatalf("CheckpointAt = %v, want %v", eng.gotResume.CheckpointAt, cpAt)
	}
}

// TestLoadAndResume_PrefersCheckpointOriginalStartedAt asserts the
// resume helper threads cp.OriginalStartedAt into the
// ResumeContext.StartedAt field, ignoring caller-supplied
// WithResumeStartedAt for resumes (only fresh runs use the
// caller-supplied value). This keeps wall-clock SLO budgets
// continuous across replays.
func TestLoadAndResume_PrefersCheckpointOriginalStartedAt(t *testing.T) {
	originalStart := time.Now().Add(-2 * time.Hour)
	store := &memStore{cp: &agent.Checkpoint{
		ExecID:            "r1",
		Steps:             []string{"step"},
		Timestamp:         time.Now().Add(-time.Hour),
		OriginalStartedAt: originalStart,
	}}

	t.Run("inherits_from_checkpoint", func(t *testing.T) {
		eng := &stubEngine{}
		callerStart := time.Now() // would-be fallback
		_, err := agent.LoadAndResume(context.Background(), eng, agent.NoopHost{}, store,
			agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil,
			agent.WithResumeStartedAt(callerStart),
		)
		if err != nil {
			t.Fatalf("LoadAndResume: %v", err)
		}
		if !eng.gotResume.StartedAt.Equal(originalStart) {
			t.Errorf("StartedAt = %v, want %v (cp.OriginalStartedAt)", eng.gotResume.StartedAt, originalStart)
		}
	})

	t.Run("falls_back_when_checkpoint_missing_field", func(t *testing.T) {
		oldCp := &agent.Checkpoint{
			ExecID:    "r1",
			Steps:     []string{"step"},
			Timestamp: time.Now().Add(-time.Hour),
		}
		oldStore := &memStore{cp: oldCp}
		eng := &stubEngine{}
		callerStart := time.Now().Add(-30 * time.Minute)
		_, err := agent.LoadAndResume(context.Background(), eng, agent.NoopHost{}, oldStore,
			agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil,
			agent.WithResumeStartedAt(callerStart),
		)
		if err != nil {
			t.Fatalf("LoadAndResume: %v", err)
		}
		if !eng.gotResume.StartedAt.Equal(callerStart) {
			t.Errorf("StartedAt = %v, want %v (caller fallback for old cp)", eng.gotResume.StartedAt, callerStart)
		}
	})
}

func TestLoadAndResume_RejectsExecIDMismatch(t *testing.T) {
	eng := &stubEngine{}
	store := &memStore{cp: &agent.Checkpoint{ExecID: "other"}}

	_, err := agent.LoadAndResume(context.Background(), eng, agent.NoopHost{}, store,
		agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("want Validation error, got %v", err)
	}
	if eng.executions != 0 {
		t.Fatal("must not call Execute on exec_id mismatch")
	}
}

func TestLoadAndResume_HonoursResumerCanResume(t *testing.T) {
	wantErr := errdefs.NotAvailable(errors.New("incompatible engine version"))
	eng := &resumerEngine{}
	eng.canResume = func(_ agent.Checkpoint) error { return wantErr }

	store := &memStore{cp: &agent.Checkpoint{ExecID: "r1"}}

	_, err := agent.LoadAndResume(context.Background(), eng, agent.NoopHost{}, store,
		agent.Run{Identity: agent.Identity{RunID: "r1"}}, nil)
	if err == nil {
		t.Fatal("expected CanResume error")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want wraps %v", err, wantErr)
	}
	if eng.executions != 0 {
		t.Fatal("must not Execute when CanResume rejects")
	}
}

func TestResumeContextFromContext_NilCtxReturnsFalse(t *testing.T) {
	if _, ok := agent.ResumeContextFromContext(nil); ok {
		t.Fatal("nil ctx must return ok=false")
	}
}

func TestRunInfoContext_RoundTrip(t *testing.T) {
	want := agent.RunInfo{
		Identity:      agent.Identity{AgentID: "a", RunID: "r"},
		ToolAllowList: []string{"search"},
	}
	ctx := agent.WithRunInfo(context.Background(), want)
	got, ok := agent.RunInfoFromContext(ctx)
	if !ok {
		t.Fatal("RunInfoFromContext ok=false after WithRunInfo")
	}
	if got.AgentID != want.AgentID || got.RunID != want.RunID ||
		len(got.ToolAllowList) != 1 || got.ToolAllowList[0] != "search" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestRunInfoFromContext_AbsentAndNil(t *testing.T) {
	if _, ok := agent.RunInfoFromContext(context.Background()); ok {
		t.Fatal("empty ctx must return ok=false")
	}
	if _, ok := agent.RunInfoFromContext(nil); ok {
		t.Fatal("nil ctx must return ok=false")
	}
}
