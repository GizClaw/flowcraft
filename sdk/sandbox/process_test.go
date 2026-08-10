package sandbox_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

func TestProcessManagerOf_Discovery(t *testing.T) {
	if pm := sandbox.ProcessManagerOf(sandbox.NewLocalRunner(t.TempDir())); pm == nil {
		t.Fatal("LocalRunner must implement ProcessManager")
	}
	if pm := sandbox.ProcessManagerOf(sandbox.NoopRunner{}); pm != nil {
		t.Fatalf("NoopRunner must not advertise sessions, got %T", pm)
	}
	if pm := sandbox.ProcessManagerOf(nil); pm != nil {
		t.Fatalf("nil runner must yield nil ProcessManager, got %T", pm)
	}

	// Decorators forward the capability; a chain over a non-PM runner
	// still answers ProcessManagerOf but fails Start with NotAvailable.
	local := sandbox.NewLocalRunner(t.TempDir())
	chained := sandbox.WithDefaults(
		sandbox.AllowCommands(local, []string{"echo"}),
		sandbox.ExecOptions{Timeout: 1},
	)
	if pm := sandbox.ProcessManagerOf(chained); pm == nil {
		t.Fatal("decorated LocalRunner must forward ProcessManager")
	}
	noopChain := sandbox.WithDefaults(sandbox.NoopRunner{}, sandbox.ExecOptions{Timeout: 1})
	if pm := sandbox.ProcessManagerOf(noopChain); pm == nil {
		t.Fatal("ProcessManagerOf on decorated runner must report the decorator")
	}
	if _, err := sandbox.ProcessManagerOf(noopChain).Start(
		context.Background(), sandbox.ProcessSpec{Argv: []string{"echo"}}); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start over NoopRunner = %v, want NotAvailable", err)
	}
}

func TestWithDefaults_ProcessManagerMergesPolicy(t *testing.T) {
	fake := &fakeRunner{pm: &fakePM{}}
	defaults := sandbox.ExecOptions{
		Timeout: 5,
		Env: sandbox.EnvPolicy{
			Allow:  []string{"DEFAULT"},
			Inject: map[string]string{"D": "d"},
		},
		Net: sandbox.NetPolicy{Mode: sandbox.NetDenyAll},
		Resources: sandbox.ResourceLimits{
			MemoryBytes: 1 << 20,
		},
	}
	rn := sandbox.WithDefaults(fake, defaults)
	pm := sandbox.ProcessManagerOf(rn)
	if pm == nil {
		t.Fatal("WithDefaults must forward ProcessManager")
	}

	_, err := pm.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"sh"},
		Opts: sandbox.ExecOptions{
			Timeout: 100,
			Env: sandbox.EnvPolicy{
				Allow:  []string{"CALLER"},
				Inject: map[string]string{"C": "c"},
			},
		},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	got := fake.pm.specs[0].Opts
	if got.Timeout != 5 {
		t.Errorf("Timeout = %v, want min(100, 5) = 5", got.Timeout)
	}
	if len(got.Env.Allow) != 1 || got.Env.Allow[0] != "DEFAULT" {
		t.Errorf("Env.Allow = %v, want defaults to win", got.Env.Allow)
	}
	if got.Env.Inject["D"] != "d" || got.Env.Inject["C"] != "c" {
		t.Errorf("Env.Inject = %v, want union with caller override", got.Env.Inject)
	}
	if got.Net.Mode != sandbox.NetDenyAll {
		t.Errorf("Net = %+v, want defaults to win", got.Net)
	}
	if got.Resources.MemoryBytes != 1<<20 {
		t.Errorf("Resources = %+v, want defaults to win", got.Resources)
	}
}

func TestWithApproval_ProcessManagerGatesStartWithTTY(t *testing.T) {
	fake := &fakeRunner{pm: &fakePM{}}
	var got []sandbox.ExecRequest
	approve := func(_ context.Context, req sandbox.ApprovalRequest) (sandbox.Decision, error) {
		got = append(got, req.Exec)
		return sandbox.Allow, nil
	}
	rn := sandbox.WithApproval(fake, approve, sandbox.CommandPatterns("sh"))
	pm := sandbox.ProcessManagerOf(rn)
	if pm == nil {
		t.Fatal("WithApproval must forward ProcessManager")
	}

	if _, err := pm.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"sh", "-c", "x"},
		TTY:  true,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("approver called %d times, want 1", len(got))
	}
	if !got[0].TTY || got[0].Command != "sh" {
		t.Fatalf("approver saw %+v, want TTY=true sh", got[0])
	}

	deny := sandbox.WithApproval(fake, func(context.Context, sandbox.ApprovalRequest) (sandbox.Decision, error) {
		return sandbox.Deny, nil
	}, sandbox.CommandPatterns("sh"))
	if _, err := sandbox.ProcessManagerOf(deny).Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"sh"},
	}); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("denied Start = %v, want PolicyDenied", err)
	}
}

func TestAllowCommands_ProcessManagerGatesStart(t *testing.T) {
	fake := &fakeRunner{pm: &fakePM{}}
	rn := sandbox.AllowCommands(fake, []string{"sh"})
	pm := sandbox.ProcessManagerOf(rn)
	if pm == nil {
		t.Fatal("AllowCommands must forward ProcessManager")
	}
	if _, err := pm.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"bash"},
	}); !errdefs.IsPolicyDenied(err) {
		t.Fatalf("non-whitelisted Start = %v, want PolicyDenied", err)
	}
	if _, err := pm.Start(context.Background(), sandbox.ProcessSpec{
		Argv: []string{"sh"},
	}); err != nil {
		t.Fatalf("whitelisted Start: %v", err)
	}
}

type fakeProcess struct{}

func (fakeProcess) ID() string { return "fake" }
func (fakeProcess) PID() int   { return 42 }
func (fakeProcess) Read(context.Context, int64, int) (sandbox.ProcessOutput, error) {
	return sandbox.ProcessOutput{}, nil
}
func (fakeProcess) Write(context.Context, []byte) error { return nil }
func (fakeProcess) Resize(context.Context, int, int) error {
	return nil
}
func (fakeProcess) Terminate(context.Context) error { return nil }
func (fakeProcess) Wait(context.Context) (sandbox.ProcessExit, error) {
	return sandbox.ProcessExit{}, nil
}
func (fakeProcess) Close() error { return nil }

type fakePM struct {
	specs []sandbox.ProcessSpec
}

func (f *fakePM) Start(_ context.Context, spec sandbox.ProcessSpec) (sandbox.Process, error) {
	f.specs = append(f.specs, spec)
	return fakeProcess{}, nil
}

func (f *fakePM) List(context.Context) ([]sandbox.ProcessInfo, error) {
	return nil, nil
}

func (f *fakePM) Terminate(context.Context, string) error {
	return nil
}

type fakeRunner struct {
	pm *fakePM
}

func (r fakeRunner) Exec(context.Context, string, []string, sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	return &sandbox.ExecResult{}, nil
}

func (r fakeRunner) Start(ctx context.Context, spec sandbox.ProcessSpec) (sandbox.Process, error) {
	return r.pm.Start(ctx, spec)
}

func (r fakeRunner) List(ctx context.Context) ([]sandbox.ProcessInfo, error) {
	return r.pm.List(ctx)
}

func (r fakeRunner) Terminate(ctx context.Context, id string) error {
	return r.pm.Terminate(ctx, id)
}
