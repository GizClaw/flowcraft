package fleet

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// rateLimitConfig declares one Daemon, one Vessel, one Agent and a
// llmRateLimits entry pinning openai-default to 2 RPM. With the
// limiter correctly threaded through catalog.Deps.LLMLimiters,
// engine factories that call Acquire before each "LLM call" observe
// the cap.
const rateLimitConfig = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-rl
spec:
  control:
    socket: /tmp/v.sock
  llmRateLimits:
    - llmProfile: openai-default
      requestsPerMinute: 2
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: support
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: helper
spec:
  engine:
    ref: rl-probe
    config:
      llmProfile: openai-default
`

// TestFleet_LLMRateLimit_DeliveredToEngine pins gap #1: the
// daemon's spec.llmRateLimits MUST reach the engine factory via
// catalog.Deps.LLMLimiters, and the engine MUST call Acquire
// before each "LLM call" so the cap actually enforces.
//
// The test wires a custom engine factory that:
//   - looks up the configured profile's Limiter from Deps,
//   - calls Acquire(ctx, vesselID, 0) before bumping a counter,
//   - returns ctx.Err on Acquire failure.
//
// With requestsPerMinute=2 and 5 concurrent Submits, the bucket
// admits the first 2 immediately and forces the rest to wait
// for the next minute window. Each Submit's ctx has a 250ms
// timeout, so the blocked ones return ctx.Err — the counter
// must observe at most 2 successful Acquires.
func TestFleet_LLMRateLimit_DeliveredToEngine(t *testing.T) {
	t.Parallel()

	const want = 2 // RequestsPerMinute
	var generates atomic.Int32

	objs, err := apispec.DecodeAll(strings.NewReader(rateLimitConfig), "<rl>")
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	cat := catalog.New()
	cat.RegisterEngine("rl-probe", func(_ string, cfg map[string]any, deps catalog.Deps) (engine.Engine, error) {
		profile, _ := cfg["llmProfile"].(string)
		limiter := deps.LLMLimiters[profile]
		// Sanity: the test asserts later that limiter is non-nil
		// — the engine factory should NEVER receive a nil limiter
		// when llmRateLimits is configured for that profile.
		return engine.EngineFunc(func(ctx context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			if limiter != nil {
				if err := limiter.Acquire(ctx, deps.VesselID, 0); err != nil {
					return b, err
				}
			}
			generates.Add(1)
			b.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleAssistant, "ok"))
			return b, nil
		}), nil
	})

	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs.Aggregate())
	}
	f, err := Build(*plan)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Stop(ctx)
	}()

	if got := f.Limiter("openai-default"); got == nil {
		t.Fatal("Fleet.Limiter returned nil — daemon plan didn't carry the LLMRateLimit entry")
	}

	var wg sync.WaitGroup
	const N = 5
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
			defer cancel()
			h, err := f.Submit(ctx, "support", "helper", agent.Request{})
			if err != nil {
				return
			}
			_, _ = h.Wait(ctx)
		}()
	}
	wg.Wait()

	if got := generates.Load(); got > int32(want) {
		t.Fatalf("Generate ran %d times within 250ms — daemon rate limit (2 RPM) not enforced; expected at most %d", got, want)
	}
	if got := generates.Load(); got != int32(want) {
		t.Fatalf("Generate ran %d times — expected exactly %d under 2 RPM cap (the bucket should admit the first %d instantly)", got, want, want)
	}
}
