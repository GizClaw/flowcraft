package engine_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestInterrupted_SatisfiesIsInterrupted(t *testing.T) {
	err := engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserCancel, Detail: "stop"})
	if !errdefs.IsInterrupted(err) {
		t.Errorf("Interrupted() error must satisfy errdefs.IsInterrupted; got %v", err)
	}
}

func TestInterrupted_AsRestoresInterrupt(t *testing.T) {
	err := engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserInput, Detail: "barge"})

	var ie engine.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatal("errors.As must destructure InterruptedError")
	}
	if ie.Cause != engine.CauseUserInput {
		t.Errorf("Cause = %q, want %q", ie.Cause, engine.CauseUserInput)
	}
	if ie.Detail != "barge" {
		t.Errorf("Detail = %q, want %q", ie.Detail, "barge")
	}
}

func TestInterrupted_AsThroughWrap(t *testing.T) {
	wrapped := fmt.Errorf("layered: %w",
		engine.Interrupted(engine.Interrupt{Cause: engine.CauseHostShutdown, Detail: "graceful"}))

	if !errdefs.IsInterrupted(wrapped) {
		t.Error("wrapped Interrupted should still satisfy IsInterrupted")
	}

	var ie engine.InterruptedError
	if !errors.As(wrapped, &ie) {
		t.Fatal("errors.As must drill through wraps")
	}
	if ie.Cause != engine.CauseHostShutdown {
		t.Errorf("Cause = %q, want %q", ie.Cause, engine.CauseHostShutdown)
	}
}

func TestInterrupted_ZeroValueWellFormedMessage(t *testing.T) {
	cases := []struct {
		name string
		intr engine.Interrupt
		want string
	}{
		{"zero", engine.Interrupt{}, "engine: interrupted"},
		{"detailOnly", engine.Interrupt{Detail: "stuck"}, "engine: interrupted: stuck"},
		{"causeOnly", engine.Interrupt{Cause: engine.CauseUserCancel}, "engine: interrupted (user_cancel)"},
		{"both", engine.Interrupt{Cause: engine.CauseUserInput, Detail: "barge"}, "engine: interrupted (user_input): barge"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := engine.Interrupted(c.intr)
			if err.Error() != c.want {
				t.Errorf("Error() = %q, want %q", err.Error(), c.want)
			}
		})
	}
}

// TestInterruptedError_MarkerInvoked ensures the unexported marker
// method on InterruptedError is actually called by an errors.As-based
// classifier; without an explicit interface assertion the cover tool
// won't see it run. We use the public marker shape that errdefs
// expects.
func TestInterruptedError_MarkerInvoked(t *testing.T) {
	err := engine.Interrupted(engine.Interrupt{Cause: engine.CauseUserCancel})

	var marker interface{ Interrupted() }
	if !errors.As(err, &marker) {
		t.Fatal("Interrupted() error must satisfy the errdefs marker shape")
	}
	// Calling the marker must not panic.
	marker.Interrupted()
}

func TestCauseConstants_StableValues(t *testing.T) {
	// The Cause string values are part of the wire contract (they
	// flow into errdefs and may be persisted in checkpoint metadata).
	// Pin them down so a refactor that renames a constant breaks
	// loudly.
	pairs := []struct {
		c    engine.Cause
		want string
	}{
		{engine.CauseUnknown, ""},
		{engine.CauseUserCancel, "user_cancel"},
		{engine.CauseUserInput, "user_input"},
		{engine.CauseHostShutdown, "host_shutdown"},
		{engine.CauseCustom, "custom"},
	}
	for _, p := range pairs {
		if string(p.c) != p.want {
			t.Errorf("Cause %q has value %q, want %q", p.c, string(p.c), p.want)
		}
	}
}
