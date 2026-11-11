package agenttest

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
)

// Steer runs the minimal steering suite against one Steerer: a message before
// the first Run belongs in Run, and a message after the terminal state reports
// that the run already ended instead of being silently queued.
func Steer(t *testing.T, build func(t *testing.T) agent.Runner) {
	t.Helper()
	runner := build(t)
	steerer, ok := runner.(agent.Steerer)
	if !ok {
		t.Fatalf("%T does not implement agent.Steerer", runner)
	}
	steer := func() error {
		_, err := steerer.Steer(context.Background(),
			message.NewTextMessage(message.RoleUser, "steer"), agent.InterruptSteer)
		return err
	}
	if err := steer(); !errors.Is(err, agent.ErrSessionIdle) {
		t.Fatalf("Steer before Run = %v, want ErrSessionIdle", err)
	}
	for i := 0; i < 4 && !runner.State().Terminal(); i++ {
		if _, err := drain(runner.Run(context.Background(), turn())); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}
	if !runner.State().Terminal() {
		t.Skipf("runner did not reach a terminal state: %s", runner.State())
	}
	if err := steer(); !errors.Is(err, agent.ErrRunEnded) {
		t.Fatalf("Steer after terminal = %v, want ErrRunEnded", err)
	}
}
