package agent_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// interruptingEngineWith returns an engine that stops with the given
// cause; used to drive disposition tests through agent.Run.
func interruptingEngineWith(cause agent.Cause) agent.Engine {
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel,
			message.NewTextMessage(message.RoleAssistant, "partial..."))
		return b, agent.Interrupted(agent.Interrupt{Cause: cause})
	})
}

func TestDiscardOnInterruptCauses_FiresOnMatch(t *testing.T) {
	dec := agent.NewDiscardOnInterruptCauses("voice_barge",
		agent.CauseUserInput, agent.CauseUserInput)

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "a"}, interruptingEngineWith(agent.CauseUserInput),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		agent.WithReferee(dec),
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
	dec := agent.NewDiscardOnInterruptCauses("voice_barge", agent.CauseUserInput)

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "a"}, interruptingEngineWith(agent.CauseHostShutdown),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		agent.WithReferee(dec),
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
	dec := agent.NewDiscardOnInterruptCauses("voice_barge", agent.CauseUserInput)

	completed := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, message.NewTextMessage(message.RoleAssistant, "ok"))
		return b, nil
	})

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "a"}, completed,
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		agent.WithReferee(dec),
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

	res, err := agent.Execute(context.Background(),
		agent.Agent{ID: "a"}, interruptingEngineWith(agent.CauseUserInput),
		agent.Request{Message: message.NewTextMessage(message.RoleUser, "hi")},
		agent.WithReferee(dec),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := res.State["finalize_reason"]; ok {
		t.Errorf("finalize_reason should not be set when causes set is empty; got %v", res.State["finalize_reason"])
	}
}
