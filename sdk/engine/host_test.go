package engine_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestNoopHost_SatisfiesHost(t *testing.T) {
	// Compile-time assertion that NoopHost still satisfies engine.Host
	// after refactors that touch the composition.
	var _ engine.Host = engine.NoopHost{}
}

func TestNoopHost_PublishDrops(t *testing.T) {
	if err := (engine.NoopHost{}).Publish(context.Background(), event.Envelope{Subject: "x"}); err != nil {
		t.Errorf("NoopHost.Publish must drop silently; got %v", err)
	}
}

func TestNoopHost_InterruptsBlocksForever(t *testing.T) {
	// We cannot assert "blocks forever" directly without a goroutine
	// leak. Asserting nil is sufficient — receiving on a nil channel
	// is the documented "blocks forever" semantic.
	if (engine.NoopHost{}).Interrupts() != nil {
		t.Error("NoopHost.Interrupts() must be nil so engines block forever on it")
	}
}

func TestNoopHost_AskUserNotAvailable(t *testing.T) {
	_, err := (engine.NoopHost{}).AskUser(context.Background(), engine.UserPrompt{})
	if !errdefs.IsNotAvailable(err) {
		t.Errorf("AskUser must return errdefs.IsNotAvailable; got %v", err)
	}
}

func TestNoopHost_CheckpointDrops(t *testing.T) {
	if err := (engine.NoopHost{}).Checkpoint(context.Background(), engine.Checkpoint{}); err != nil {
		t.Errorf("NoopHost.Checkpoint must drop silently; got %v", err)
	}
}

func TestNoopHost_ReportUsageDropsAndReturnsNil(t *testing.T) {
	// NoopHost has no budget so it MUST return nil — engines that
	// branch on errdefs.IsBudgetExceeded never see it under noop.
	if err := (engine.NoopHost{}).ReportUsage(context.Background(),
		model.TokenUsage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}); err != nil {
		t.Errorf("NoopHost.ReportUsage must return nil; got %v", err)
	}
}

func TestEngineFunc_NilSafe(t *testing.T) {
	// Documented contract: a zero-value EngineFunc returns
	// (board, nil) without panicking.
	board := engine.NewBoard()
	got, err := engine.EngineFunc(nil).Execute(
		context.Background(), engine.Run{}, engine.NoopHost{}, board)
	if err != nil {
		t.Errorf("nil EngineFunc.Execute returned error: %v", err)
	}
	if got != board {
		t.Error("nil EngineFunc.Execute must echo the input board")
	}
}

func TestEngineFunc_AdaptsClosure(t *testing.T) {
	called := false
	f := engine.EngineFunc(func(_ context.Context, r engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
		called = true
		if r.ID != "exec-1" {
			t.Errorf("Run.ID = %q, want exec-1", r.ID)
		}
		return b, nil
	})

	b := engine.NewBoard()
	_, err := f.Execute(context.Background(), engine.Run{ID: "exec-1"}, engine.NoopHost{}, b)
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if !called {
		t.Error("EngineFunc did not invoke wrapped closure")
	}
}
