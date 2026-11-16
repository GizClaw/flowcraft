package runtime

import (
	"context"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/permission"
	"github.com/GizClaw/flowcraft/core/sandbox"
	sandboxengine "github.com/GizClaw/flowcraft/core/sandbox/engine"
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
		NetModes:         permission.IsolatingNetModes(),
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

// policyChain runs the two build steps the runtime runs — resolve the document,
// then fix the defaults onto the calls — so a test exercises the same projection
// and enforcement the assembly does.
func policyChain(t *testing.T, rt *Runtime, inner *policyRunner, limits sandbox.ResourceLimits) (sandbox.Runner, error) {
	t.Helper()
	spec, err := rt.resolveSandbox(context.Background(), limits, inner.Capabilities())
	if err != nil {
		return nil, err
	}
	return rt.applySandboxPolicy(inner, spec)
}

// The resolved view a decorator receives carries the effective workdir and the
// write boundary the composed chain applies, so a companion chain does not
// re-derive the document — and cannot silently root itself elsewhere.
func TestResolveSandboxCarriesTheResolvedView(t *testing.T) {
	root := t.TempDir()
	workDir := t.TempDir()
	rt := policyRuntime(t, SandboxConfig{
		WorkDir: workDir,
		Timeout: "5s",
		Writes:  []string{root},
		Net:     NetConfig{Mode: "none"},
	})
	inner := &policyRunner{caps: confiningCaps()}

	spec, err := rt.resolveSandbox(context.Background(), sandbox.ResourceLimits{}, inner.Capabilities())
	if err != nil {
		t.Fatalf("resolveSandbox: %v", err)
	}
	if spec.WorkDir != workDir {
		t.Errorf("WorkDir = %q, want sandbox.workdir %q", spec.WorkDir, workDir)
	}
	if spec.Defaults.Timeout != 5*time.Second {
		t.Errorf("Defaults.Timeout = %v, want 5s", spec.Defaults.Timeout)
	}
	if spec.Defaults.Permission == nil {
		t.Fatal("Defaults.Permission = nil; the configured writes did not reach the view")
	}
	if paths := spec.Defaults.Permission.Writes.Paths; len(paths) != 1 || paths[0] != root {
		t.Errorf("Defaults.Permission.Writes = %v, want [%s]", paths, root)
	}
	if spec.Defaults.Net.Mode != permission.NetDenyAll {
		t.Errorf("Defaults.Net.Mode = %v, want the document's NetDenyAll", spec.Defaults.Net.Mode)
	}
	if spec.Config.Sandbox.Backend != BackendLocal {
		t.Errorf("Config backend = %q, want the resolved document", spec.Config.Sandbox.Backend)
	}
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

	runner, err := policyChain(t, rt, inner, sandbox.ResourceLimits{
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

// sandbox.net is a posture the deployment states once, and engine.WithDefaults
// replaces the tool's per-call value with the chain's default (defaults win
// entirely). Without projecting it here the parsed policy was handed down and
// then overwritten with "host default", so the backend never saw it.
func TestSandboxNetPolicyReachesEnforcingBackends(t *testing.T) {
	cases := []struct {
		name string
		net  NetConfig
		want permission.NetMode
	}{
		{
			name: "allow_list",
			net:  NetConfig{Mode: "allow_list", AllowHosts: []string{"api.example.com"}},
			want: permission.NetAllowList,
		},
		{
			name: "proxy",
			net:  NetConfig{Mode: "proxy", Proxy: "socks5://proxy.example:1080"},
			want: permission.NetProxy,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt := policyRuntime(t, SandboxConfig{Net: tc.net})
			inner := &policyRunner{caps: confiningCaps()}

			runner, err := policyChain(t, rt, inner, sandbox.ResourceLimits{})
			if err != nil {
				t.Fatalf("policyChain: %v", err)
			}
			opts := startThrough(t, inner, runner, sandbox.ExecOptions{})
			if opts.Net.Mode != tc.want {
				t.Fatalf("Net.Mode = %v, want %v", opts.Net.Mode, tc.want)
			}
			if tc.net.AllowHosts != nil &&
				(len(opts.Net.AllowHosts) != 1 || opts.Net.AllowHosts[0] != tc.net.AllowHosts[0]) {
				t.Fatalf("Net.AllowHosts = %v, want %v", opts.Net.AllowHosts, tc.net.AllowHosts)
			}
			if tc.net.Proxy != "" && opts.Net.Proxy != tc.net.Proxy {
				t.Fatalf("Net.Proxy = %q, want %q", opts.Net.Proxy, tc.net.Proxy)
			}
		})
	}
}

// Without a backend that can enforce the mode the call still runs: the local
// runner has no OS boundary and rejects such a call rather than running it
// unconfined, so the value is not projected.
func TestSandboxNetPolicyStaysAdvisoryWithoutEnforcement(t *testing.T) {
	rt := policyRuntime(t, SandboxConfig{Net: NetConfig{Mode: "none"}})
	inner := &policyRunner{caps: sandbox.Capabilities{}}

	runner, err := policyChain(t, rt, inner, sandbox.ResourceLimits{})
	if err != nil {
		t.Fatalf("policyChain: %v", err)
	}
	if opts := startThrough(t, inner, runner, sandbox.ExecOptions{}); opts.Net.Mode != permission.NetDefault {
		t.Fatalf("Net.Mode = %v, want the zero posture on a backend that cannot enforce one", opts.Net.Mode)
	}
}

// A real local runtime keeps running with an unenforceable net block: the
// advisory path must not turn every command into NotAvailable.
func TestSandboxNetPolicyKeepsLocalCommandsRunning(t *testing.T) {
	if goruntime.GOOS == "windows" {
		t.Skip("uses a POSIX shell")
	}
	cfg := validConfig(t.TempDir())
	cfg.Sandbox.Net = NetConfig{Mode: "none"}

	rt, err := New(t.Context(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = rt.Close() }()

	result, err := sandboxengine.Exec(t.Context(), rt.Runner(), "/bin/sh",
		[]string{"-c", "exit 0"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
}

// A remote runner reports the server's declaration verbatim and the wire carries
// Net, so the capability gate answers for it exactly as it does locally: a server
// that declares the mode receives the policy, and one that cannot is advisory.
func TestSandboxNetPolicyFollowsTheRemoteDeclaration(t *testing.T) {
	cases := []struct {
		name string
		caps sandbox.Capabilities
		want permission.NetMode
	}{
		{name: "server declares the mode", caps: confiningCaps(), want: permission.NetDenyAll},
		{name: "server cannot enforce it", caps: sandbox.Capabilities{}, want: permission.NetDefault},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig(t.TempDir())
			cfg.Sandbox = SandboxConfig{
				Backend: BackendRemote,
				Net:     NetConfig{Mode: "none"},
				Remote:  RemoteConfig{Endpoint: "https://sandbox.example"},
			}
			rt := &Runtime{cfg: cfg}
			inner := &policyRunner{caps: tc.caps}

			runner, err := policyChain(t, rt, inner, sandbox.ResourceLimits{})
			if err != nil {
				t.Fatalf("policyChain: %v", err)
			}
			if opts := startThrough(t, inner, runner, sandbox.ExecOptions{}); opts.Net.Mode != tc.want {
				t.Fatalf("Net.Mode = %v, want %v", opts.Net.Mode, tc.want)
			}
		})
	}
}

// A backend that cannot measure a budget is a build error, not a dropped field:
// running unmeasured is the failure the cap exists to prevent.
func TestSandboxPolicyRejectsUnenforceableCaps(t *testing.T) {
	rt := policyRuntime(t, SandboxConfig{Timeout: "1s", MemoryMiB: 64})
	inner := &policyRunner{caps: sandbox.Capabilities{Policy: sandbox.Enforcement{}}}
	_, err := policyChain(t, rt, inner, sandbox.ResourceLimits{MemoryBytes: 64 << 20})
	if !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation", err)
	}
	if !strings.Contains(err.Error(), "sandbox.memory_mib") {
		t.Errorf("error %q does not name the field", err)
	}
	// applySandboxPolicy does not own the runner: the caller closes it on error,
	// which buildRunner does.

	rt = policyRuntime(t, SandboxConfig{Timeout: "1s", CPU: 500})
	cpuInner := &policyRunner{caps: sandbox.Capabilities{}}
	_, err = policyChain(t, rt, cpuInner, sandbox.ResourceLimits{CPUMillicores: 500})
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

	runner, err := policyChain(t, rt, inner, sandbox.ResourceLimits{})
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
	runner, err := policyChain(t, rt, inner, sandbox.ResourceLimits{})
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
