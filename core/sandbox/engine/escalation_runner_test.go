package engine_test

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// refusalResult is what a confined attempt returns when the OS sandbox refused
// the command: non-zero exit plus a marker Denied recognizes.
var refusalResult = &sandbox.ExecResult{
	ExitCode: 1,
	Stderr:   "sed: couldn't open file: Operation not permitted",
}

// countingExecutor is one chain of an escalation: it records what reached it so
// a test can prove which chain ran and that a session never leaves the confine.
type countingExecutor struct {
	result *sandbox.ExecResult

	execCalls  int
	startCalls int
	closeCalls int
	lastOpts   sandbox.ExecOptions
}

func (r *countingExecutor) Exec(
	_ context.Context, _ string, _ []string, opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	r.execCalls++
	r.lastOpts = opts
	return r.result, nil
}

func (r *countingExecutor) Start(
	context.Context, sandbox.SessionSpec,
) (sandbox.Session, error) {
	r.startCalls++
	return nil, errForwarded
}

func (r *countingExecutor) List(context.Context) ([]sandbox.SessionInfo, error) {
	return nil, errForwarded
}

func (r *countingExecutor) Terminate(context.Context, string) error { return errForwarded }
func (r *countingExecutor) Capabilities() sandbox.Capabilities      { return sandbox.Capabilities{} }
func (r *countingExecutor) Close() error {
	r.closeCalls++
	return nil
}

// An approved refusal re-runs the whole command through the retry chain, and
// engine.Exec reaches this flow only because the runner implements Exec: the
// same call must arrive byte for byte on both chains.
func TestEscalationRunnerRetriesThroughExec(t *testing.T) {
	confined := &countingExecutor{result: refusalResult}
	unconfined := &countingExecutor{result: &sandbox.ExecResult{Stdout: "retried\n"}}
	escalator := &fakeEscalator{decision: engine.EscalationDecision{Allow: true}}
	escalation := &engine.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
		Escalate:   escalator,
		Gate:       fixedGate(true),
	}

	var outcomes []engine.EscalationOutcome
	runner, err := engine.NewEscalationRunner(escalation,
		engine.WithEscalationObserver(func(outcome engine.EscalationOutcome) {
			outcomes = append(outcomes, outcome)
		}))
	if err != nil {
		t.Fatalf("NewEscalationRunner: %v", err)
	}

	opts := sandbox.ExecOptions{WorkDir: "/work", Timeout: 5 * time.Second}
	result, err := engine.Exec(
		context.Background(), runner, "sed", []string{"-i", "s/a/b/", "f"}, opts)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "retried\n" {
		t.Fatalf("result = %+v, want the retry's result", result)
	}
	if confined.execCalls != 1 || unconfined.execCalls != 1 {
		t.Fatalf("exec calls: confined = %d, retry = %d; want 1 and 1",
			confined.execCalls, unconfined.execCalls)
	}
	if !reflect.DeepEqual(confined.lastOpts, unconfined.lastOpts) {
		t.Fatalf("retry options = %+v, want the confined attempt's %+v",
			unconfined.lastOpts, confined.lastOpts)
	}
	if len(escalator.requests) != 1 {
		t.Fatalf("prompts = %d, want 1", len(escalator.requests))
	}
	if len(outcomes) != 1 || !outcomes[0].Refused || !outcomes[0].Approved {
		t.Fatalf("outcomes = %+v, want one Refused+Approved", outcomes)
	}

	// A call the sandbox allows reports the zero outcome, so a host can count
	// every one-shot call, not just the escalated ones.
	confined.result = &sandbox.ExecResult{}
	if _, err := engine.Exec(context.Background(), runner, "true", nil, sandbox.ExecOptions{}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(outcomes) != 2 || outcomes[1] != (engine.EscalationOutcome{}) {
		t.Fatalf("outcomes = %+v, want a zero outcome for the allowed call", outcomes)
	}
}

// Sessions never escalate: an interactive session is refused over many reads,
// so the session calls stay on the confined chain and the retry chain is never
// touched.
func TestEscalationRunnerKeepsSessionsConfined(t *testing.T) {
	confined := &countingExecutor{}
	unconfined := &countingExecutor{}
	runner, err := engine.NewEscalationRunner(&engine.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
	})
	if err != nil {
		t.Fatalf("NewEscalationRunner: %v", err)
	}

	if _, err := runner.Start(context.Background(), sandbox.SessionSpec{Argv: []string{"sh"}}); !errors.Is(err, errForwarded) {
		t.Fatalf("Start err = %v, want the confined chain's %v", err, errForwarded)
	}
	if confined.startCalls != 1 || unconfined.startCalls != 0 {
		t.Fatalf("start calls: confined = %d, retry = %d; want 1 and 0",
			confined.startCalls, unconfined.startCalls)
	}
}

// The runner owns the retry chain the host built and forwards the confined one,
// so a single Close releases both exactly once.
func TestEscalationRunnerClosesBothChains(t *testing.T) {
	confined := &countingExecutor{}
	unconfined := &countingExecutor{}
	runner, err := engine.NewEscalationRunner(&engine.Escalation{
		Confined:   confined,
		Unconfined: unconfined,
	})
	if err != nil {
		t.Fatalf("NewEscalationRunner: %v", err)
	}

	if err := runner.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if confined.closeCalls != 1 || unconfined.closeCalls != 1 {
		t.Fatalf("close calls: confined = %d, retry = %d; want 1 and 1",
			confined.closeCalls, unconfined.closeCalls)
	}
}

func TestNewEscalationRunnerRequiresBothChains(t *testing.T) {
	cases := []struct {
		name string
		esc  *engine.Escalation
	}{
		{name: "nil escalation"},
		{name: "no confined", esc: &engine.Escalation{Unconfined: &countingExecutor{}}},
		{name: "no unconfined", esc: &engine.Escalation{Confined: &countingExecutor{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := engine.NewEscalationRunner(tc.esc); !errdefs.IsValidation(err) {
				t.Fatalf("err = %v, want a validation error", err)
			}
		})
	}
}
