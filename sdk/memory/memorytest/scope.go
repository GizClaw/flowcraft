package memorytest

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/memory"
)

// ScopeSuite drives the documented Scope contract: Validate
// enforces RuntimeID, HardPartitionKey combines RuntimeID and
// UserID with a NUL separator, IsZero is the structural zero of
// the struct. None of these need an impl, so the suite has no
// BuildRuntime hook.
type ScopeSuite struct{}

func RunScope(t *testing.T, _ ScopeSuite) {
	t.Helper()

	t.Run("Validate_rejects_empty_RuntimeID", func(t *testing.T) {
		err := memory.Scope{}.Validate()
		if err == nil {
			t.Fatal("expected error, got nil")
		}
		memErr := memory.AsError(err)
		if memErr == nil || memErr.Kind != memory.KindScopeInvalid {
			t.Errorf("expected *Error KindScopeInvalid, got: %T %v", err, err)
		}
	})

	t.Run("Validate_accepts_only_RuntimeID", func(t *testing.T) {
		if err := (memory.Scope{RuntimeID: "prod"}).Validate(); err != nil {
			t.Errorf("global scope must validate, got: %v", err)
		}
	})

	t.Run("Validate_accepts_full_scope", func(t *testing.T) {
		s := memory.Scope{
			RuntimeID:      "prod",
			UserID:         "tenant-1",
			AgentID:        "researcher",
			ConversationID: "conv-42",
			DatasetID:      "kb",
		}
		if err := s.Validate(); err != nil {
			t.Errorf("full scope must validate, got: %v", err)
		}
	})

	t.Run("HardPartitionKey_uses_NUL_separator", func(t *testing.T) {
		got := (memory.Scope{RuntimeID: "prod", UserID: "u"}).HardPartitionKey()
		want := "prod\x00u"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("HardPartitionKey_global_scope", func(t *testing.T) {
		got := (memory.Scope{RuntimeID: "prod"}).HardPartitionKey()
		if got != "prod\x00" {
			t.Errorf("got %q, want %q", got, "prod\x00")
		}
	})

	t.Run("HardPartitionKey_distinguishes_users", func(t *testing.T) {
		a := (memory.Scope{RuntimeID: "prod", UserID: "u1"}).HardPartitionKey()
		b := (memory.Scope{RuntimeID: "prod", UserID: "u2"}).HardPartitionKey()
		if a == b {
			t.Errorf("different users produced the same key: %q", a)
		}
	})

	t.Run("IsZero_structural_zero", func(t *testing.T) {
		if !(memory.Scope{}).IsZero() {
			t.Error("Scope{} should be the zero value")
		}
		// The global override (RuntimeID set, others empty) is
		// valid but not zero.
		if (memory.Scope{RuntimeID: "prod"}).IsZero() {
			t.Error("Scope{RuntimeID: prod} is not the zero value")
		}
	})

	t.Run("String_includes_set_fields", func(t *testing.T) {
		s := memory.Scope{RuntimeID: "prod", UserID: "u1", ConversationID: "c"}
		out := s.String()
		for _, want := range []string{"rt=prod", "user=u1", "conv=c"} {
			if !strings.Contains(out, want) {
				t.Errorf("String() = %q, missing %q", out, want)
			}
		}
	})
}
