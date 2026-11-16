package engine_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
)

// executorRunner opts into the one-shot override: Exec answers and Start fails,
// so a test can tell which path engine.Exec took.
type executorRunner struct {
	result  *sandbox.ExecResult
	execErr error

	execCalls  int
	startCalls int
	gotCmd     string
	gotArgs    []string
	gotOpts    sandbox.ExecOptions
}

func (r *executorRunner) Exec(
	_ context.Context, cmd string, args []string, opts sandbox.ExecOptions,
) (*sandbox.ExecResult, error) {
	r.execCalls++
	r.gotCmd, r.gotArgs, r.gotOpts = cmd, args, opts
	return r.result, r.execErr
}

func (r *executorRunner) Start(
	context.Context, sandbox.SessionSpec,
) (sandbox.Session, error) {
	r.startCalls++
	return nil, errors.New("engine.Exec must not drain a session when the runner implements Exec")
}

func (r *executorRunner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }
func (r *executorRunner) Terminate(context.Context, string) error             { return nil }
func (r *executorRunner) Capabilities() sandbox.Capabilities                  { return sandbox.Capabilities{} }
func (r *executorRunner) Close() error                                        { return nil }

// The one-shot form is a derived view over Start, but a decorator that needs the
// completed result owns the whole contract: engine.Exec routes to its Exec and
// hands over the caller's options untouched, so the wrapper sees the call the
// caller made rather than one the platform rewrote on the way down.
func TestExecDispatchesToExecutor(t *testing.T) {
	runner := &executorRunner{result: &sandbox.ExecResult{ExitCode: 3, Stdout: "canned"}}
	opts := sandbox.ExecOptions{
		WorkDir:   "/work",
		Stdin:     []byte("input"),
		Timeout:   3 * time.Second,
		Resources: sandbox.ResourceLimits{MaxOutputBytes: 4096},
	}

	result, err := engine.Exec(
		context.Background(), runner, "sh", []string{"-c", "true"}, opts)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 3 || result.Stdout != "canned" {
		t.Fatalf("result = %+v, want the executor's own result", result)
	}
	if runner.execCalls != 1 || runner.startCalls != 0 {
		t.Fatalf("Exec calls = %d, Start calls = %d; want 1 and 0",
			runner.execCalls, runner.startCalls)
	}
	if runner.gotCmd != "sh" || !slices.Equal(runner.gotArgs, []string{"-c", "true"}) {
		t.Fatalf("argv = %q %q", runner.gotCmd, runner.gotArgs)
	}
	if runner.gotOpts.WorkDir != opts.WorkDir ||
		runner.gotOpts.Timeout != opts.Timeout ||
		string(runner.gotOpts.Stdin) != string(opts.Stdin) ||
		runner.gotOpts.Resources.MaxOutputBytes != opts.Resources.MaxOutputBytes {
		t.Fatalf("options = %+v, want %+v", runner.gotOpts, opts)
	}
}

// The override owns the error too: engine.Exec must surface it as-is rather
// than retrying the drain.
func TestExecReturnsExecutorError(t *testing.T) {
	boom := errors.New("boom")
	runner := &executorRunner{execErr: boom}
	if _, err := engine.Exec(context.Background(), runner, "true", nil, sandbox.ExecOptions{}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if runner.startCalls != 0 {
		t.Fatalf("Start calls = %d, want 0", runner.startCalls)
	}
}

// A runner that does not implement the optional capability keeps the derived
// view over Start: nothing about the default path changed.
func TestExecKeepsDerivedViewWithoutExecutor(t *testing.T) {
	_, err := engine.Exec(context.Background(), stubRunner{}, "true", nil, sandbox.ExecOptions{})
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err = %v, want NotAvailable from the Start-derived path", err)
	}
}
