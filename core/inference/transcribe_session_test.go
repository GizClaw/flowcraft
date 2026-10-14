package inference

import (
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
)

// TestTranscriptionSessionTerminalViolationIsUnusable pins the terminal
// classification of the session path: whoever reaches finishResultLocked with
// a transcript the contract rejects gets the same attempt-void kind the unary
// path reports, not corruption. Per-event validation usually catches a
// violation earlier, so the terminal check is the net behind it, and this
// pins that the net reports the same class.
func TestTranscriptionSessionTerminalViolationIsUnusable(t *testing.T) {
	session := &decodedTranscriptionSession[TranscriptionSessionEvent]{
		model:     ModelRef{ID: ModelID{Provider: "fake", Name: "transcribe"}},
		startedAt: time.Now(),
		segments:  []TranscriptionSegment{{}},
	}
	session.finishResultLocked()

	err := session.resultErr
	if err == nil {
		t.Fatal("finishResultLocked accepted a transcript with an empty segment")
	}
	var inferenceErr *Error
	if !errors.As(err, &inferenceErr) {
		t.Fatalf("resultErr = %v, want *inference.Error", err)
	}
	if inferenceErr.Kind != ProviderResponseUnusable {
		t.Fatalf("kind = %q, want %q", inferenceErr.Kind, ProviderResponseUnusable)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("error = %v, want not available (attempt void)", err)
	}
	if errdefs.IsInternal(err) {
		t.Fatal("an unusable transcript must not classify as internal corruption")
	}
}
