package retrieval

import (
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestPartialErrorEmptyMessage(t *testing.T) {
	var e *PartialError = &PartialError{}
	if got := e.Error(); got != "retrieval: partial upsert" {
		t.Fatalf("empty: got %q", got)
	}
}

func TestPartialErrorOnlyReportsFailures(t *testing.T) {
	e := &PartialError{Results: []DocUpsertResult{
		{ID: "ok"},
		{ID: "bad", Err: errdefs.Validationf("boom")},
		{ID: "also-ok"},
	}}
	got := e.Error()
	if !strings.Contains(got, "bad: ") {
		t.Fatalf("expected failure id in message, got %q", got)
	}
	if strings.Contains(got, "ok") && !strings.Contains(got, "also-ok") {
		// "ok" appears as a substring of "also-ok"; ensure neither id with
		// nil error leaks into the user-facing message.
		t.Fatalf("expected successful ids omitted, got %q", got)
	}
	if strings.Contains(got, "; ; ") {
		t.Fatalf("unexpected empty separator in %q", got)
	}
}

func TestErrNoQueryIsValidation(t *testing.T) {
	if !errdefs.IsValidation(ErrNoQuery) {
		t.Fatalf("ErrNoQuery should classify as validation")
	}
	if !errors.Is(ErrNoQuery, ErrNoQuery) {
		t.Fatalf("errors.Is should match the sentinel itself")
	}
}
