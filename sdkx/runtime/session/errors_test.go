package session

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestSessionErrorsRemainDistinctWhenWrapped(t *testing.T) {
	all := []error{ErrSessionClosed, ErrPromptUnknown, ErrPromptDuplicate, ErrPromptClosed, ErrSinkQueueFull}
	for i, target := range all {
		wrapped := fmt.Errorf("operation failed: %w", target)
		if !errors.Is(wrapped, target) {
			t.Fatalf("errors.Is did not match %v", target)
		}
		for j, other := range all {
			if i != j && errors.Is(wrapped, other) {
				t.Fatalf("%v unexpectedly matched %v", target, other)
			}
		}
	}
}

func TestPublicBoundaryErrorsCarryClassifications(t *testing.T) {
	turn := &Turn{done: make(chan struct{})}
	if _, err := turn.Wait(nil); !errdefs.IsValidation(err) {
		t.Fatalf("Wait(nil) error = %v, want validation", err)
	}
	if err := turn.Reply(nil, "prompt", agent.UserReply{}); !errdefs.IsValidation(err) {
		t.Fatalf("Reply(nil) error = %v, want validation", err)
	}
	if _, err := turn.askUser(nil, agent.UserPrompt{}); !errdefs.IsValidation(err) {
		t.Fatalf("askUser(nil) error = %v, want validation", err)
	}
	if _, err := (&Session{}).Start(nil, agent.Request{}); !errdefs.IsValidation(err) {
		t.Fatalf("Start(nil) error = %v, want validation", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := turn.Wait(ctx); !errdefs.IsAborted(err) {
		t.Fatalf("Wait(canceled) error = %v, want aborted", err)
	}
	if _, err := (&Manager{}).open(ctx, Key{AgentID: "agent", ContextID: "context"}); !errdefs.IsAborted(err) {
		t.Fatalf("Manager.open(canceled) error = %v, want aborted", err)
	}
}

func TestSessionErrorsCarryBehavioralClassifications(t *testing.T) {
	tests := []struct {
		err   error
		check func(error) bool
	}{
		{ErrManagerClosed, errdefs.IsNotAvailable},
		{ErrSessionClosed, errdefs.IsNotAvailable},
		{ErrPromptUnknown, errdefs.IsNotFound},
		{ErrPromptDuplicate, errdefs.IsConflict},
		{ErrPromptClosed, errdefs.IsNotAvailable},
		{ErrSinkQueueFull, errdefs.IsBudgetExceeded},
	}
	for _, test := range tests {
		if !test.check(fmt.Errorf("wrapped: %w", test.err)) {
			t.Errorf("%v has the wrong classification", test.err)
		}
	}
}
