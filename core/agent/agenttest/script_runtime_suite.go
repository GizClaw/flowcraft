package agenttest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
)

// ScriptRuntimeFactory builds a fresh [agent.ScriptRuntime] for each
// subtest.
type ScriptRuntimeFactory func() agent.ScriptRuntime

// ScriptFixture describes a minimal script the runtime under test must
// execute without any host bindings. The source is passed verbatim to
// [agent.ScriptRuntime.Exec]; the suite only requires it to complete
// without an error (a nil *ScriptSignal is a valid plain completion).
type ScriptFixture struct {
	// Source is a minimal runnable script. The zero value (empty
	// source) is a valid trivial script for both bundled runtimes.
	Source string
}

// ScriptRuntimeSuite runs every contract probe that applies to the
// [agent.ScriptRuntime] produced by f:
//
//   - zero inputs must not panic (an error is acceptable);
//   - the declared fixture must execute without an error;
//   - Exec MUST be safe for concurrent use.
func ScriptRuntimeSuite(t *testing.T, f ScriptRuntimeFactory, fixture ScriptFixture) {
	t.Helper()

	t.Run("ZeroInputNoPanic", func(t *testing.T) { scriptZeroInput(t, f) })
	t.Run("FixtureCompletes", func(t *testing.T) { scriptFixture(t, f, fixture) })
	t.Run("ConcurrentSafe", func(t *testing.T) { scriptConcurrent(t, f, fixture) })
}

// ---------- subtests ----------

func scriptZeroInput(t *testing.T, f ScriptRuntimeFactory) {
	t.Helper()
	r := f()
	defer recoverPanicAs(t, "Exec(zero inputs)")
	_, _ = r.Exec(context.Background(), "", "", &agent.ScriptEnv{})
}

func scriptFixture(t *testing.T, f ScriptRuntimeFactory, fixture ScriptFixture) {
	t.Helper()
	r := f()
	defer recoverPanicAs(t, "Exec(fixture)")
	if _, err := r.Exec(context.Background(), "agenttest", fixture.Source, &agent.ScriptEnv{}); err != nil {
		t.Errorf("Exec(fixture) = %v; the declared minimal script must execute without error", err)
	}
}

func scriptConcurrent(t *testing.T, f ScriptRuntimeFactory, fixture ScriptFixture) {
	t.Helper()
	r := f()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("concurrent Exec panicked: %v", r)
				}
			}()
			_, _ = r.Exec(ctx, "agenttest", fixture.Source, &agent.ScriptEnv{})
		}()
	}
	wg.Wait()
}
