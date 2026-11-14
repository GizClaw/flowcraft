package agent

import (
	"context"
	"iter"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime/runctx"
)

// captureRunner records the run identity every Run receives.
type captureRunner struct {
	mu     sync.Mutex
	seen   []runctx.Run
	state  TaskState
	pauses int
}

func (r *captureRunner) State() TaskState {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.state
}

func (r *captureRunner) Run(ctx context.Context, _ message.Message) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		run, _ := runctx.From(ctx)
		r.mu.Lock()
		r.seen = append(r.seen, run)
		pause := r.pauses > 0
		if pause {
			r.pauses--
			r.state = StateInputRequired
		} else {
			r.state = StateCompleted
		}
		state := r.state
		r.mu.Unlock()
		yield(Status{State: state}, nil)
	}
}

func (r *captureRunner) Cancel(context.Context, string) error { return nil }
func (r *captureRunner) Close() error                         { return nil }

func runIdentity(t *testing.T, pauses int) []runctx.Run {
	t.Helper()
	runner := &captureRunner{state: StateSubmitted, pauses: pauses}
	agent, err := NewAgent(AgentOptions{
		NewRunner: func(context.Context, string, string) (Runner, error) {
			return runner, nil
		},
		RunContext: func(ctx context.Context, sessionID string) context.Context {
			return runctx.WithRun(ctx, runctx.Run{
				SessionID: sessionID,
				AgentName: "writer",
			})
		},
	})
	if err != nil {
		t.Fatalf("NewAgent: %v", err)
	}
	defer func() { _ = agent.Close() }()

	run := func() {
		for event, runErr := range agent.Run(context.Background(), "s-1",
			message.NewTextMessage(message.RoleUser, "hi")) {
			if runErr != nil {
				t.Fatalf("Run: %v", runErr)
			}
			_ = event
		}
	}
	run()
	run()

	runner.mu.Lock()
	defer runner.mu.Unlock()
	return append([]runctx.Run(nil), runner.seen...)
}

func TestRunAttachesTaskIDAndTurn(t *testing.T) {
	seen := runIdentity(t, 0)
	if len(seen) != 2 {
		t.Fatalf("runs = %d, want 2", len(seen))
	}
	if seen[0].SessionID != "s-1" || seen[0].AgentName != "writer" {
		t.Fatalf("first run = %+v", seen[0])
	}
	if seen[0].TaskID == "" || seen[1].TaskID == "" {
		t.Fatalf("task ids are empty: %+v", seen)
	}
	if seen[0].TaskID == seen[1].TaskID {
		t.Fatalf("a terminal task must get a new task id: %+v", seen)
	}
	if seen[0].Turn != 1 || seen[1].Turn != 2 {
		t.Fatalf("turns = %d/%d, want 1/2", seen[0].Turn, seen[1].Turn)
	}
}

func TestRunResumeKeepsTheTaskID(t *testing.T) {
	seen := runIdentity(t, 1)
	if len(seen) != 2 {
		t.Fatalf("runs = %d, want 2", len(seen))
	}
	if seen[0].TaskID != seen[1].TaskID {
		t.Fatalf("resume changed the task id: %+v", seen)
	}
	if seen[0].Turn != 1 || seen[1].Turn != 2 {
		t.Fatalf("turns = %d/%d, want 1/2", seen[0].Turn, seen[1].Turn)
	}
}
