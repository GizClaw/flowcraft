package script_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/script"
)

// SignalToError is the single point of truth that host code (scriptnode,
// scriptengine, …) uses to translate script-side signals into Go errors.
// These tests cover the full classification matrix so a regression in
// the mapping is impossible to merge silently.

func TestSignalToError_NilAndDone(t *testing.T) {
	cases := []*script.Signal{
		nil,
		{Type: ""},
		{Type: "done"},
		{Type: "done", Kind: "ignored", Message: "ignored"},
		// Unknown types intentionally return nil — only "error" and
		// "interrupt" cross the host boundary as observable failures.
		{Type: "weird"},
	}
	for _, sig := range cases {
		if err := script.SignalToError(sig); err != nil {
			t.Errorf("SignalToError(%+v) = %v, want nil", sig, err)
		}
	}
}

func TestSignalToError_ErrorKinds(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		check   func(error) bool
		wantMsg string
	}{
		{"validation", "validation", errdefs.IsValidation, "bad input"},
		{"not_found", "not_found", errdefs.IsNotFound, "no such thing"},
		{"budget_exceeded", "budget_exceeded", errdefs.IsBudgetExceeded, "out of tokens"},
		{"policy_denied", "policy_denied", errdefs.IsPolicyDenied, "tool blocked"},
		{"not_available", "not_available", errdefs.IsNotAvailable, "knowledge down"},
		{"internal_explicit", "internal", errdefs.IsInternal, "boom"},
		{"internal_default_empty_kind", "", errdefs.IsInternal, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := script.SignalToError(&script.Signal{
				Type:    "error",
				Kind:    tt.kind,
				Message: tt.wantMsg,
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !tt.check(err) {
				t.Errorf("classification check failed for %q: %v", tt.kind, err)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error message %q missing %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestSignalToError_UnknownKindDegradesToInternal(t *testing.T) {
	err := script.SignalToError(&script.Signal{
		Type:    "error",
		Kind:    "typo-from-script",
		Message: "oops",
	})
	if !errdefs.IsInternal(err) {
		t.Fatalf("expected internal classification, got %v", err)
	}
	// The raw kind value must survive in the chain so observability
	// surfaces the typo instead of swallowing it.
	if !strings.Contains(err.Error(), "typo-from-script") {
		t.Errorf("error message should preserve unknown kind, got %q", err.Error())
	}
}

func TestSignalToError_ErrorEmptyMessage(t *testing.T) {
	// A script that does signal.error({kind: "validation"}) without a
	// message still classifies — we just substitute a placeholder so
	// the error string is non-empty.
	err := script.SignalToError(&script.Signal{Type: "error", Kind: "validation"})
	if !errdefs.IsValidation(err) {
		t.Fatalf("expected validation, got %v", err)
	}
	if err.Error() == "" {
		t.Error("error string should never be empty")
	}
}

func TestSignalToError_InterruptCauseMapping(t *testing.T) {
	tests := []struct {
		kind      string
		wantCause engine.Cause
	}{
		{"user_cancel", engine.CauseUserCancel},
		{"user_input", engine.CauseUserInput},
		{"host_shutdown", engine.CauseHostShutdown},
		{"custom", engine.CauseCustom},
		{"", engine.CauseCustom},        // empty → custom
		{"unknown", engine.CauseCustom}, // unknown → custom (no synthetic causes)
	}
	for _, tt := range tests {
		t.Run(tt.kind, func(t *testing.T) {
			err := script.SignalToError(&script.Signal{
				Type:    "interrupt",
				Kind:    tt.kind,
				Message: "pausing",
			})
			if !errdefs.IsInterrupted(err) {
				t.Fatalf("expected IsInterrupted, got %v", err)
			}
			var ie engine.InterruptedError
			if !errors.As(err, &ie) {
				t.Fatalf("expected to extract InterruptedError from %v", err)
			}
			if ie.Cause != tt.wantCause {
				t.Errorf("Cause = %q, want %q", ie.Cause, tt.wantCause)
			}
			if ie.Detail != "pausing" {
				t.Errorf("Detail = %q, want %q", ie.Detail, "pausing")
			}
		})
	}
}

func TestSignalToError_BackCompatBareMessage(t *testing.T) {
	// Pre-PR scripts that called signal.error("boom") with no kind
	// must still produce a classifiable error (Internal) so existing
	// callers keep working without source changes.
	err := script.SignalToError(&script.Signal{Type: "error", Message: "boom"})
	if !errdefs.IsInternal(err) {
		t.Fatalf("expected internal for empty-kind error, got %v", err)
	}

	// And signal.interrupt("...") must keep producing an Interrupted
	// engine error with CauseCustom.
	err = script.SignalToError(&script.Signal{Type: "interrupt", Message: "pause"})
	var ie engine.InterruptedError
	if !errors.As(err, &ie) {
		t.Fatalf("expected interrupted, got %v", err)
	}
	if ie.Cause != engine.CauseCustom {
		t.Errorf("Cause = %q, want %q", ie.Cause, engine.CauseCustom)
	}
}
