package tool_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// countingRecorder is a [tool.CallRecorder] whose identity is the assertion: the
// pointer that reaches the domain is the one a run attached.
type countingRecorder struct {
	calls []message.ToolCall
}

func (r *countingRecorder) RecordCall(call message.ToolCall) { r.calls = append(r.calls, call) }

func TestScopeFrom_AbsentAndAttached(t *testing.T) {
	if _, ok := tool.ScopeFrom(context.Background()); ok {
		t.Fatal("ScopeFrom outside a run should report absent")
	}
	if _, ok := tool.ScopeFrom(nil); ok {
		t.Fatal("ScopeFrom on a nil context should report absent")
	}

	rec := &countingRecorder{}
	ctx := tool.WithScope(context.Background(), tool.Scope{Recorder: rec})
	scope, ok := tool.ScopeFrom(ctx)
	if !ok || scope.Recorder != rec {
		t.Fatalf("ScopeFrom = %+v, %v; want the attached recorder", scope, ok)
	}
}

// EnsureScope never shadows the projection a run already attached: a caller that
// finds no recorder below it must not swap the run's view for a fresh one, which
// would discard the run's discovery state.
func TestEnsureScope_DoesNotShadow(t *testing.T) {
	rec := &countingRecorder{}
	ctx := tool.WithScope(context.Background(), tool.Scope{Recorder: rec})

	kept := tool.EnsureScope(ctx, &countingRecorder{})
	if scope, ok := tool.ScopeFrom(kept); !ok || scope.Recorder != rec {
		t.Fatalf("EnsureScope shadowed the run's scope: %+v, %v", scope, ok)
	}
}

func TestEnsureScope_AttachesWhenAbsent(t *testing.T) {
	rec := &countingRecorder{}
	ctx := tool.EnsureScope(context.Background(), rec)
	if scope, ok := tool.ScopeFrom(ctx); !ok || scope.Recorder != rec {
		t.Fatalf("EnsureScope did not attach the recorder: %+v, %v", scope, ok)
	}

	base := context.Background()
	if got := tool.EnsureScope(base, nil); got != base {
		t.Fatal("EnsureScope with a nil recorder should return the context unchanged")
	}
}

// The deprecated recorder accessors are forwarders over the domain: a caller that
// still spells the old name reaches the same scope, and attaching a second
// recorder reuses the first for the same reason EnsureScope does.
func TestCallRecorder_ForwardersAgreeWithScope(t *testing.T) {
	first := &countingRecorder{}
	//nolint:staticcheck // deprecated forwarder: pinned so it keeps reaching the domain
	ctx := tool.WithCallRecorder(context.Background(), first)
	if scope, ok := tool.ScopeFrom(ctx); !ok || scope.Recorder != first {
		t.Fatalf("WithCallRecorder did not write the scope: %+v, %v", scope, ok)
	}

	second := &countingRecorder{}
	//nolint:staticcheck // deprecated forwarder: pinned so it keeps EnsureScope's behavior
	kept := tool.WithCallRecorder(ctx, second)
	//nolint:staticcheck // deprecated forwarder: reads the same scope
	if got, ok := tool.CallRecorderFrom(kept); !ok || got != first {
		t.Fatalf("WithCallRecorder shadowed the run's recorder: %v, %v", got, ok)
	}
	//nolint:staticcheck // deprecated forwarder: absent scope stays absent
	if _, ok := tool.CallRecorderFrom(context.Background()); ok {
		t.Fatal("CallRecorderFrom outside a run should report absent")
	}
}
