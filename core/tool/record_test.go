package tool_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/core/tool"
)

// TestClassifyFailure pins the vocabulary mapping: a tool that honours its context is
// not reporting a tool bug, and the dashboards group by this value.
func TestClassifyFailure(t *testing.T) {
	for name, want := range map[string]tool.FailureKind{
		"deadline":       tool.FailureTimeout,
		"wrapped cancel": tool.FailureCanceled,
		"plain":          tool.FailureToolError,
	} {
		var err error
		switch name {
		case "deadline":
			err = context.DeadlineExceeded
		case "wrapped cancel":
			err = errors.Join(errors.New("inner"), context.Canceled)
		default:
			err = errors.New("boom")
		}
		if got := tool.ClassifyFailure(err); got != want {
			t.Fatalf("%s: ClassifyFailure = %q, want %q", name, got, want)
		}
	}
}

// TestFailureRecordingIsFirstWins is the rule that keeps a cause from being rewritten by
// an outer stage: a timeout that wrapped a policy denial must not replace it.
func TestFailureRecordingIsFirstWins(t *testing.T) {
	ctx := tool.WithFailureRecorder(context.Background())
	if _, ok := tool.FailureKindFrom(ctx); ok {
		t.Fatal("a fresh recorder already carries a classification")
	}

	tool.RecordFailure(ctx, tool.FailurePolicyDenied)
	tool.RecordFailure(ctx, tool.FailureTimeout)
	kind, ok := tool.FailureKindFrom(ctx)
	if !ok || kind != tool.FailurePolicyDenied {
		t.Fatalf("kind = %q (ok=%v), want the first classification", kind, ok)
	}

	// An empty classification is not a classification.
	tool.RecordFailure(tool.WithFailureRecorder(context.Background()), "")
}

func TestRecordFailureWithoutARecorderIsANoOp(t *testing.T) {
	tool.RecordFailure(context.Background(), tool.FailureTimeout)
	if _, ok := tool.FailureKindFrom(context.Background()); ok {
		t.Fatal("a bare context reported a classification")
	}
}

// TestRecorderIsInstalledOnce keeps an inner stage from shadowing the executor's box: a
// middleware that called WithFailureRecorder again would otherwise lose every
// classification made before it.
func TestRecorderIsInstalledOnce(t *testing.T) {
	ctx := tool.WithFailureRecorder(context.Background())
	inner := tool.WithFailureRecorder(ctx)
	tool.RecordFailure(ctx, tool.FailurePanicked)
	if kind, ok := tool.FailureKindFrom(inner); !ok || kind != tool.FailurePanicked {
		t.Fatalf("inner context did not see the classification: %q (ok=%v)", kind, ok)
	}
}

func TestCallIDRoundTripsAndKeepsTheBox(t *testing.T) {
	ctx := tool.WithFailureRecorder(context.Background())
	ctx = tool.WithCallID(ctx, "call-7")

	id, ok := tool.CallIDFromContext(ctx)
	if !ok || id != "call-7" {
		t.Fatalf("CallIDFromContext = (%q, %v)", id, ok)
	}
	// The id travels with the box, not instead of it.
	tool.RecordFailure(ctx, tool.FailureNotFound)
	if kind, ok := tool.FailureKindFrom(ctx); !ok || kind != tool.FailureNotFound {
		t.Fatalf("the box was lost when the call id was set: %q (ok=%v)", kind, ok)
	}
}

func TestCallIDWithoutACallIsUnknown(t *testing.T) {
	if id, ok := tool.CallIDFromContext(context.Background()); ok {
		t.Fatalf("a bare context reported call id %q", id)
	}
	if id, ok := tool.CallIDFromContext(tool.WithFailureRecorder(context.Background())); ok {
		t.Fatalf("a recorder without a call reported call id %q", id)
	}
}
