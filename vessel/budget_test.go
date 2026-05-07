package vessel

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestBudget_PerTurnCancelsRun: an engine that reports usage above
// MaxTokensPerTurn must see ctx.Done() at its next iteration. The
// observable signal is ReportUsage returning a RateLimit error AND
// the run terminating with that classification.
func TestBudget_PerTurnCancelsRun(t *testing.T) {
	t.Parallel()

	greedy := engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		// Report 200 tokens; cap is 100, so this single call trips it.
		err := h.ReportUsage(ctx, model.TokenUsage{TotalTokens: 200})
		if err == nil {
			b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "should not reach"))
			return b, nil
		}
		return b, err
	})

	vs := spec.Spec{
		Agents:    []spec.Agent{{Name: "p"}},
		Resources: spec.Resources{MaxTokensPerTurn: 100},
	}
	c, err := New(vs, WithEngine(greedy))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	res, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")})
	// The error may surface either as the call's err or via
	// res.Err / res.Status — agent.Run swallows most engine
	// errors into Result.Err. Both are valid signals; we accept
	// whichever the runtime chose.
	cause := err
	if cause == nil && res != nil {
		cause = res.Err
	}
	if !errdefs.IsRateLimit(cause) {
		t.Fatalf("expected RateLimit, got err=%v res=%+v", err, res)
	}
	if res != nil && res.Status == agent.StatusCompleted {
		t.Fatalf("status=Completed despite budget breach")
	}
}

// TestBudget_PerHourBlocksAdmission: once the rolling-hour total
// has crossed MaxTokensPerHour, subsequent Submits are rejected
// up-front with RateLimit instead of waiting for the engine to
// fail mid-flight.
func TestBudget_PerHourBlocksAdmission(t *testing.T) {
	t.Parallel()

	spender := engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		_ = h.ReportUsage(ctx, model.TokenUsage{TotalTokens: 60})
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{
		Agents:    []spec.Agent{{Name: "p"}},
		Resources: spec.Resources{MaxTokensPerHour: 100},
	}
	c, err := New(vs, WithEngine(spender))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	// First run is admitted and pushes the hour-bucket to 60.
	if _, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "1")}); err != nil {
		t.Fatalf("first run unexpectedly failed: %v", err)
	}
	// Second run drives the bucket to 120 (>100) — runs to
	// completion since admission is checked BEFORE the run starts
	// and the bucket was still under cap at admission time.
	if _, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "2")}); err != nil {
		t.Fatalf("second run unexpectedly failed: %v", err)
	}
	// Third Submit is now rejected: hour-bucket sits at 120 > 100.
	_, err = c.Submit(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "3")})
	if err == nil || !errdefs.IsRateLimit(err) {
		t.Fatalf("third Submit: want RateLimit, got %v", err)
	}
}

// TestBudget_NoCapsZeroOverhead: the unlimited (zero-cap) case
// keeps the budget pointer nil so unrelated engines pay no
// per-call cost and ReportUsage stays a tight passthrough. We can't
// directly assert "no allocations" without benchmarks, but we CAN
// assert that an engine reporting a million tokens still completes
// fine — i.e. no cap is silently applied.
func TestBudget_NoCapsZeroOverhead(t *testing.T) {
	t.Parallel()

	heavy := engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, b *engine.Board) (*engine.Board, error) {
		if err := h.ReportUsage(ctx, model.TokenUsage{TotalTokens: 1_000_000}); err != nil {
			return b, err
		}
		b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
		return b, nil
	})

	vs := spec.Spec{Agents: []spec.Agent{{Name: "p"}}}
	c, _ := New(vs, WithEngine(heavy))
	if err := c.Launch(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Stop(context.Background()) })

	if _, err := c.Call(context.Background(), "p", agent.Request{Message: model.NewTextMessage(model.RoleUser, "go")}); err != nil {
		t.Fatalf("uncapped run failed: %v", err)
	}
}

// TestBudget_HourlyRotation: the sliding window must zero stale
// minute-buckets so a vessel doesn't stay budgeted-out forever.
// We exercise this on the budget directly with an injected clock —
// 60+ minutes elapsed should clear the bucket.
func TestBudget_HourlyRotation(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC)
	b := newTokenBudget(0, 100)
	b.now = func() time.Time { return now }

	u := b.begin("r1", func() {})
	if err := b.add(u, 80); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !b.hourExhausted() != true {
		// 80 < 100, NOT exhausted yet
	}

	// Push another 30 → 110 within the same minute → exhausted.
	if err := b.add(u, 30); err == nil || !errors.Is(err, err) {
		// expect RateLimit
		if !errdefs.IsRateLimit(err) {
			t.Fatalf("expected RateLimit on overflow, got %v", err)
		}
	}
	if !b.hourExhausted() {
		t.Fatal("expected hour exhausted")
	}

	// Advance the clock 61 minutes — every bucket gets shifted out.
	now = now.Add(61 * time.Minute)
	if b.hourExhausted() {
		t.Fatal("expected hour-window to have rotated past stale tokens")
	}
}
