package vesselquality

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

// TestFleet_NCaptainsConcurrent stands up N=20 independent
// Captains, each with its own fake LLM, then dispatches K Calls
// in parallel across the fleet. The assertion is twofold:
//
//  1. Every Call completes successfully — no captain drops a
//     request, races on shared state, or leaks a goroutine into
//     another vessel's run.
//  2. The wall-clock budget is reasonable: with cap=4
//     concurrent runs per vessel and a 10ms LLM delay, the
//     theoretical floor for 20 vessels × 5 calls each is well
//     under 1s. We grant 5s as a generous CI ceiling.
//
// This is the closest in-process analogue of the future Fleet
// daemon stress test; the e2e variant (N vesseld processes) is
// follow-up work since it needs a separate orchestrator.
func TestFleet_NCaptainsConcurrent(t *testing.T) {
	t.Parallel()
	const N = 20
	const K = 5

	caps := make([]*vessel.Captain, N)
	for i := 0; i < N; i++ {
		fake := fakellm.New([]fakellm.Step{
			{Text: fmt.Sprintf("v%d-ok", i), Delay: 10 * time.Millisecond},
		}, fakellm.WithRepeatLast())
		vs := spec.Spec{
			ID:        fmt.Sprintf("v-fleet-%d", i),
			Agents:    []spec.Agent{{Name: "primary"}},
			Resources: spec.Resources{MaxConcurrentRuns: 4},
		}
		caps[i] = launchedCaptain(t, vs,
			vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 2)),
		)
	}

	var (
		ok   atomic.Int32
		fail atomic.Int32
	)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < N; i++ {
		for j := 0; j < K; j++ {
			wg.Add(1)
			go func(c *vessel.Captain) {
				defer wg.Done()
				res, err := c.Call(context.Background(), "primary", agent.Request{
					Message: model.NewTextMessage(model.RoleUser, "go"),
				})
				if err != nil || res.Status != agent.StatusCompleted {
					fail.Add(1)
					return
				}
				ok.Add(1)
			}(caps[i])
		}
	}
	wg.Wait()
	elapsed := time.Since(start)

	total := int32(N * K)
	if ok.Load() != total {
		t.Fatalf("fleet stress: ok=%d fail=%d total=%d", ok.Load(), fail.Load(), total)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("fleet stress took %s — exceeds CI budget", elapsed)
	}
	t.Logf("fleet ok: N=%d × K=%d in %s", N, K, elapsed)
}
