package agent_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

type suggestingEngine struct {
	calls int
	err   error
}

func (suggestingEngine) Execute(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
	return b, nil
}

func (s *suggestingEngine) SuggestCheckpoint() error {
	s.calls++
	return s.err
}

func TestSuggestCheckpoint_NoOpForEnginesWithoutInterface(t *testing.T) {
	plain := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, nil
	})
	if err := agent.SuggestCheckpoint(plain); err != nil {
		t.Fatalf("SuggestCheckpoint on a plain engine must be a nil-returning no-op; got %v", err)
	}
}

func TestSuggestCheckpoint_DelegatesToImplementation(t *testing.T) {
	se := &suggestingEngine{err: errors.New("snapshot failed")}
	got := agent.SuggestCheckpoint(se)
	if se.calls != 1 {
		t.Fatalf("SuggestCheckpoint must call the engine once; got %d", se.calls)
	}
	if got == nil || got.Error() != "snapshot failed" {
		t.Fatalf("SuggestCheckpoint must surface the engine error; got %v", got)
	}
}
