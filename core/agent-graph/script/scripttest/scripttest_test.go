package scripttest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent-graph/script/scripttest"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

// stubBackend is a backend without a language: the source is a JSON object of
// writes, a "$key" string copies that read key, "fail:" raises and "slow:"
// reports an exhausted budget.
//
// It exists to prove the suite runs and can fail: a suite nobody executes is a
// claim, not a check.
type stubBackend struct{}

func (stubBackend) Name() string { return "test" }

func (stubBackend) Capabilities() script.Capabilities {
	return script.Capabilities{Deterministic: true, Terminating: true, MemoryBounded: true}
}

func (stubBackend) Compile(_ context.Context, source script.Script) (script.Program, error) {
	return stubProgram{source: source}, nil
}

func (stubBackend) Run(
	_ context.Context,
	program script.Program,
	state map[string]any,
	_ script.Budget,
	_ script.Host,
) (map[string]any, error) {
	compiled, ok := program.(stubProgram)
	if !ok {
		return nil, fmt.Errorf("wrong program type %T", program)
	}
	text := strings.TrimSpace(compiled.source.Text)
	switch {
	case strings.HasPrefix(text, "fail:"):
		return nil, fmt.Errorf("%s", strings.TrimPrefix(text, "fail:"))
	case strings.HasPrefix(text, "slow:"):
		return nil, errdefs.BudgetExceededf("%s", strings.TrimPrefix(text, "slow:"))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, fmt.Errorf("decode source: %w", err)
	}
	for key, value := range out {
		ref, ok := value.(string)
		if !isReference(ref, ok) {
			continue
		}
		out[key] = state[strings.TrimPrefix(ref, "$")]
	}
	return out, nil
}

func isReference(ref string, ok bool) bool { return ok && strings.HasPrefix(ref, "$") }

type stubProgram struct{ source script.Script }

func (p stubProgram) Version() string { return p.source.Version }

// TestStubBackendSatisfiesTheContract runs the suite against the stub, which is
// what a real backend does in its own package.
func TestStubBackendSatisfiesTheContract(t *testing.T) {
	scripttest.Run(t, scripttest.Fixture{
		Backend:   stubBackend{},
		Echo:      `{"game":"$game","answer":"$answer"}`,
		Extra:     `{"game":"$game","answer":"$answer","leak":true}`,
		WrongKind: `{"answer":"$game"}`,
		Broken:    "fail:the rule refused this state",
		Slow:      "slow:exhausted",
	})
}

// TestSuiteCatchesABrokenBackend proves the suite fails when a backend breaks one
// promise. A conformance suite that only passes is indistinguishable from no
// suite at all.
func TestSuiteCatchesABrokenBackend(t *testing.T) {
	broken := &recordingBackend{stubBackend{}}
	// Drop the memory capability claim but keep asking for a memory budget in a
	// case the suite never runs — instead, break the echo source so the round trip
	// cannot pass: the suite must reject the backend, not skip it.
	failures := runSuite(t, scripttest.Fixture{
		Backend:   broken,
		Echo:      `{"game":"$game"}`, // the answer key is missing from the result
		Extra:     `{"game":"$game","answer":"$answer","leak":true}`,
		WrongKind: `{"answer":"$game"}`,
		Broken:    "fail:boom",
		Slow:      "slow:exhausted",
	})
	if !failures {
		t.Fatal("the suite passed a backend that lost a declared write")
	}
}

type recordingBackend struct{ inner stubBackend }

func (r *recordingBackend) Name() string                      { return r.inner.Name() }
func (r *recordingBackend) Capabilities() script.Capabilities { return r.inner.Capabilities() }
func (r *recordingBackend) Compile(ctx context.Context, source script.Script) (script.Program, error) {
	return r.inner.Compile(ctx, source)
}
func (r *recordingBackend) Run(
	ctx context.Context,
	program script.Program,
	state map[string]any,
	budget script.Budget,
	host script.Host,
) (map[string]any, error) {
	return r.inner.Run(ctx, program, state, budget, host)
}

// runSuite runs the conformance suite against a sub-test and reports whether it
// failed, which is how this file checks the suite's own teeth without making the
// package's own tests fail.
func runSuite(t *testing.T, fixture scripttest.Fixture) (failed bool) {
	t.Helper()
	internal := &testing.T{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if recover() != nil {
				failed = true
			}
		}()
		scripttest.Run(internal, fixture)
		failed = internal.Failed()
	}()
	<-done
	return failed
}
