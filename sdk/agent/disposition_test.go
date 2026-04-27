package agent_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// interruptingEngineWith returns an engine that stops with the given
// cause; used to drive disposition tests through agent.Run.
func interruptingEngineWith(cause engine.Cause) engine.Engine {
	return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel,
			model.NewTextMessage(model.RoleAssistant, "partial..."))
		return b, engine.Interrupted(engine.Interrupt{Cause: cause})
	})
}

func TestDiscardOnInterruptCauses_FiresOnMatch(t *testing.T) {
	dec := agent.NewDiscardOnInterruptCauses("voice_barge",
		engine.CauseUserInput, engine.CauseUserCancel)

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "a"}, interruptingEngineWith(engine.CauseUserInput),
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")},
		agent.WithDecider(dec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Committed {
		t.Error("matching cause should leave Committed=false")
	}
	if got := res.State["finalize_reason"]; got != "voice_barge" {
		t.Errorf("finalize_reason = %v, want %q", got, "voice_barge")
	}
}

func TestDiscardOnInterruptCauses_SkipsForeignCause(t *testing.T) {
	dec := agent.NewDiscardOnInterruptCauses("voice_barge", engine.CauseUserInput)

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "a"}, interruptingEngineWith(engine.CauseHostShutdown),
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")},
		agent.WithDecider(dec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Default disposition still discards interrupted runs (Committed
	// stays false), but the decider must NOT have written a reason.
	if _, ok := res.State["finalize_reason"]; ok {
		t.Errorf("finalize_reason should be absent when decider did not fire; got %v", res.State["finalize_reason"])
	}
}

func TestDiscardOnInterruptCauses_NotInterruptedDoesNotFire(t *testing.T) {
	dec := agent.NewDiscardOnInterruptCauses("voice_barge", engine.CauseUserInput)

	completed := engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "a"}, completed,
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")},
		agent.WithDecider(dec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Committed {
		t.Error("non-interrupted run should remain Committed=true")
	}
	if _, ok := res.State["finalize_reason"]; ok {
		t.Errorf("finalize_reason should be absent on non-interrupted runs")
	}
}

func TestDiscardOnInterruptCauses_ZeroValueMatchesNothing(t *testing.T) {
	// NewDiscardOnInterruptCauses with no causes is permitted but
	// noisy: it never fires. Verify the no-op behaviour.
	dec := agent.NewDiscardOnInterruptCauses("noop")

	res, err := agent.Run(context.Background(),
		agent.Agent{ID: "a"}, interruptingEngineWith(engine.CauseUserInput),
		agent.Request{Message: model.NewTextMessage(model.RoleUser, "hi")},
		agent.WithDecider(dec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := res.State["finalize_reason"]; ok {
		t.Errorf("finalize_reason should not be set when causes set is empty; got %v", res.State["finalize_reason"])
	}
}
