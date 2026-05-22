package recall

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

// These tests pin the public-boundary error contract for sdk/recall
// v2: Save / Recall / Forget input validation must be classifiable
// as errdefs.Validation so HTTP/gRPC shims map to 400 without
// each caller pattern-matching error text.

func TestSave_MissingRuntimeID_IsValidation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	_, err = mem.Save(context.Background(), Scope{}, SaveRequest{})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id on Save must map to Validation: %v", err)
	}
}

func TestRecall_MissingRuntimeID_IsValidation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	_, err = mem.Recall(context.Background(), Scope{}, Query{})
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("missing runtime_id on Recall must map to Validation: %v", err)
	}
}

func TestForget_EmptyFactID_IsValidation(t *testing.T) {
	mem, err := New()
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	defer mem.Close()
	err = mem.Forget(context.Background(), Scope{RuntimeID: "rt", UserID: "u"}, "")
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errdefs.IsValidation(err) {
		t.Errorf("empty fact id on Forget must map to Validation: %v", err)
	}
}
