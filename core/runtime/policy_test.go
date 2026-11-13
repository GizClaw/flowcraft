package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/sandbox"
)

// policyRunner records what reached the backend, so "the document takes effect"
// is an assertion about ExecOptions rather than a claim about intent.
type policyRunner struct {
	caps    sandbox.Capabilities
	started []sandbox.ExecOptions
	closed  bool
}

func (p *policyRunner) Close() error {
	p.closed = true
	return nil
}

func (p *policyRunner) Capabilities() sandbox.Capabilities { return p.caps }

func (p *policyRunner) Start(_ context.Context, spec sandbox.SessionSpec) (sandbox.Session, error) {
	p.started = append(p.started, spec.Opts)
	return nil, errdefs.NotAvailablef("policyRunner: no sessions")
}

func (p *policyRunner) List(context.Context) ([]sandbox.SessionInfo, error) { return nil, nil }
func (p *policyRunner) Terminate(context.Context, string) error             { return nil }

// confiningCaps is a backend that enforces everything the document can ask for.
func confiningCaps() sandbox.Capabilities {
	return sandbox.Capabilities{Policy: sandbox.Enforcement{
		EnvAllowList:     true,
		MemoryCap:        true,
		CPUCap:           true,
		WriteConfinement: true,
		FilesystemBounds: true,
	}}
}

func policyRuntime(t *testing.T, sandboxCfg SandboxConfig) *Runtime {
	t.Helper()
	cfg := validConfig(t.TempDir())
	cfg.Sandbox = sandboxCfg
	cfg.Sandbox.Backend = BackendLocal
	return &Runtime{cfg: cfg}
}

// startThrough drives one Start through the decorated runner and returns what the
// backend saw. The recorder refuses sessions; only the options matter here.
func startThrough(t *testing.T, rec *policyRunner, runner sandbox.Runner, opts sandbox.ExecOptions) sandbox.ExecOptions {
	t.Helper()
	_, err := runner.Start(context.Background(), sandbox.SessionSpec{
		Argv: []string{"true"},
		Opts: opts,
	})
	if err != nil && !errdefs.IsNotAvailable(err) {
		t.Fatalf("Start: %v", err)
	}
	if len(rec.started) == 0 {
		t.Fatal("the backend never saw the start")
	}
	return rec.started[len(rec.started)-1]
}

// The document's cpu and memory budgets reach every call, and its writes become
// the per-call boundary on a backend that can confine them.
func TestSandboxPolicyReachesEveryCall(t *testing.T) {
	root := t.TempDir()
	rt := policyRuntime(t, SandboxConfig{
		Timeout:   "45s",
		CPU:       1500,
		MemoryMiB: 256,
		Writes:    []string{root},
	})
	inner := &policyRunner{caps: confiningCaps()}

	runner, err := rt.applySandboxPolicy(context.Background(), inner, sandbox.ResourceLimits{
		CPUMillicores: 1500,
		MemoryBytes:   256 << 20,
	})
	if err != nil {
		t.Fatalf("applySandboxPolicy: %v", err)
	}

	opts := startThrough(t, inner, runner, sandbox.ExecOptions{})
	if opts.Resources.CPUMillicores != 1500 {
		t.Errorf("CPUMillicores = %d, want 1500", opts.Resources.CPUMillicores)
	}
	if opts.Resources.MemoryBytes != 256<<20 {
		t.Errorf("MemoryBytes = %d, want %d", opts.Resources.MemoryBytes, 256<<20)
	}
	if opts.Timeout != 45*time.Second {
		t.Errorf("Timeout = %v, want 45s", opts.Timeout)
	}
	if opts.Permission == nil || len(opts.Permission.Writes.Paths) != 1 || opts.Permission.Writes.Paths[0] != root {
		t.Fatalf("Permission = %+v, want the configured write boundary", opts.Permission)
	}
}

// A backend that cannot measure a budget is a build error, not a dropped field:
// running unmeasured is the failure the cap exists to prevent.
func TestSandboxPolicyRejectsUnenforceableCaps(t *testing.T) {
	rt := policyRuntime(t, SandboxConfig{Timeout: "1s", MemoryMiB: 64})
	inner := &policyRunner{caps: sandbox.Capabilities{Policy: sandbox.Enforcement{}}}
	_, err := rt.applySandboxPolicy(context.Background(), inner, sandbox.ResourceLimits{MemoryBytes: 64 << 20})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "sandbox.memory_mib") {
		t.Errorf("error %q does not name the field", err)
	}
	// applySandboxPolicy does not own the runner: the caller closes it on error,
	// which buildRunner does.

	rt = policyRuntime(t, SandboxConfig{Timeout: "1s", CPU: 500})
	_, err = rt.applySandboxPolicy(context.Background(), &policyRunner{caps: sandbox.Capabilities{}}, sandbox.ResourceLimits{CPUMillicores: 500})
	if !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "sandbox.cpu_millicores") {
		t.Fatalf("err = %v, want a Validation naming sandbox.cpu_millicores", err)
	}
}

// Without write confinement the write list is advisory: the call must still run,
// but with no boundary rather than a boundary nothing enforces.
func TestSandboxPolicyLeavesWritesAdvisoryWithoutConfinement(t *testing.T) {
	rt := policyRuntime(t, SandboxConfig{Writes: []string{t.TempDir()}})
	caps := confiningCaps()
	caps.Policy.WriteConfinement = false
	caps.Policy.FilesystemBounds = false
	inner := &policyRunner{caps: caps}

	runner, err := rt.applySandboxPolicy(context.Background(), inner, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("applySandboxPolicy: %v", err)
	}
	if got := startThrough(t, inner, runner, sandbox.ExecOptions{}); got.Permission != nil {
		t.Fatalf("Permission = %+v, want nil on a backend with no write confinement", got.Permission)
	}
}

// The timeout is a ceiling: a call that asks for a shorter deadline keeps it.
func TestSandboxPolicyTimeoutIsACeiling(t *testing.T) {
	rt := policyRuntime(t, SandboxConfig{Timeout: "30s"})
	inner := &policyRunner{caps: confiningCaps()}
	runner, err := rt.applySandboxPolicy(context.Background(), inner, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("applySandboxPolicy: %v", err)
	}
	if got := startThrough(t, inner, runner, sandbox.ExecOptions{Timeout: 2 * time.Second}).Timeout; got != 2*time.Second {
		t.Fatalf("Timeout = %v, want the caller's shorter 2s", got)
	}
}

// The config rules that make the projection possible are validated at load time.
func TestSandboxConfigTimeoutAndCPURules(t *testing.T) {
	cfg := validConfig(t.TempDir())
	cfg.Sandbox = SandboxConfig{Backend: BackendLocal, Timeout: "nope"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "sandbox.timeout") {
		t.Fatalf("err = %v, want a sandbox.timeout validation", err)
	}

	cfg = validConfig(t.TempDir())
	cfg.Sandbox = SandboxConfig{Backend: BackendLocal, CPU: 500}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "sandbox.timeout") {
		t.Fatalf("cpu without timeout: err = %v, want a sandbox.timeout requirement", err)
	}

	cfg = validConfig(t.TempDir())
	cfg.Sandbox = SandboxConfig{Backend: BackendLocal, CPU: 500, Timeout: "10s"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("cpu with timeout rejected: %v", err)
	}
}
