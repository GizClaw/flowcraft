package vesselquality

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

type countingObserver struct {
	agent.BaseObserver
	starts int32
	ends   int32
	lastID atomic.Value // string
}

func (o *countingObserver) OnRunStart(_ context.Context, info agent.RunInfo, _ *agent.Request) {
	atomic.AddInt32(&o.starts, 1)
	o.lastID.Store(info.RunID)
}

func (o *countingObserver) OnRunEnd(_ context.Context, _ agent.RunInfo, _ *agent.Result) {
	atomic.AddInt32(&o.ends, 1)
}

// TestWithObserver_FiresPerRun asserts the vessel-level observer
// registered via WithObserver receives OnRunStart + OnRunEnd
// exactly once per Call.
func TestWithObserver_FiresPerRun(t *testing.T) {
	t.Parallel()
	fake := fakellm.New([]fakellm.Step{{Text: "ok"}}, fakellm.WithRepeatLast())
	obs := &countingObserver{}

	vs := spec.Spec{
		ID:     "v-obs",
		Agents: []spec.Agent{{Name: "primary"}},
	}
	c := launchedCaptain(t, vs,
		vessel.WithObserver(obs),
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 2)),
	)

	for i := 0; i < 3; i++ {
		if _, err := c.Call(context.Background(), "primary", agent.Request{
			Message: model.NewTextMessage(model.RoleUser, "hi"),
		}); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt32(&obs.starts); got != 3 {
		t.Errorf("OnRunStart count = %d, want 3", got)
	}
	if got := atomic.LoadInt32(&obs.ends); got != 3 {
		t.Errorf("OnRunEnd count = %d, want 3", got)
	}
	if id, _ := obs.lastID.Load().(string); id == "" {
		t.Errorf("RunID was empty in OnRunStart")
	}
}

type discardDecider struct {
	calls int32
}

func (d *discardDecider) BeforeFinalize(_ context.Context, _ agent.RunInfo, _ *agent.Request, _ *agent.Result) (agent.FinalizeDecision, error) {
	atomic.AddInt32(&d.calls, 1)
	return agent.FinalizeDecision{DiscardOutput: true, Reason: "test-discard"}, nil
}

// TestWithDecider_DiscardsOutput asserts a Decider returning
// DiscardOutput=true causes Result.Committed=false even on a
// successful StatusCompleted run, and the assistant message is
// dropped from any history append (no historyAppender writes).
func TestWithDecider_DiscardsOutput(t *testing.T) {
	t.Parallel()
	fake := fakellm.New([]fakellm.Step{{Text: "would-be-committed"}}, fakellm.WithRepeatLast())
	dec := &discardDecider{}

	vs := spec.Spec{
		ID: "v-dec",
		Agents: []spec.Agent{
			{Name: "primary", HistoryAccess: spec.HistoryAccessReadWrite},
		},
		History: &spec.History{Kind: "buffer", MaxMessages: 16},
	}
	c := launchedCaptain(t, vs,
		vessel.WithDecider(dec),
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 2)),
	)

	res, err := c.Call(context.Background(), "primary", agent.Request{
		ContextID: "conv-dec",
		Message:   model.NewTextMessage(model.RoleUser, "q1"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %s, want completed", res.Status)
	}
	if res.Committed {
		t.Fatalf("Committed = true, want false (decider asked to discard)")
	}
	if got := atomic.LoadInt32(&dec.calls); got != 1 {
		t.Errorf("Decider calls = %d, want 1", got)
	}

	// A second Call MUST NOT see the discarded "would-be-committed"
	// reply in its replayed transcript.
	if _, err := c.Call(context.Background(), "primary", agent.Request{
		ContextID: "conv-dec",
		Message:   model.NewTextMessage(model.RoleUser, "q2"),
	}); err != nil {
		t.Fatalf("Call2: %v", err)
	}
	calls := fake.Calls()
	if len(calls) < 2 {
		t.Fatalf("LLM calls = %d, want >= 2", len(calls))
	}
	for _, m := range calls[1].Messages {
		if m.Content() == "would-be-committed" {
			t.Fatalf("discarded reply leaked into next turn's transcript")
		}
	}
}
