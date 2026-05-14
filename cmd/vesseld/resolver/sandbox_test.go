package resolver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec/v1alpha1"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
)

// TestResolveSandboxes_LocalBackend exercises the happy path for
// the local backend. Side-effect verification (a real Exec inside
// the resolved runner) lives here rather than in a fleet-level
// test so a regression in the resolver's WithDefaults wiring is
// not hidden behind cross-package failures.
func TestResolveSandboxes_LocalBackend(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	in := map[string]v1alpha1.Sandbox{
		"dev": {
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindSandbox},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "dev"},
			Spec: v1alpha1.SandboxSpec{
				Backend: "local",
				RootDir: root,
				Env: &v1alpha1.SandboxEnv{
					Inject: map[string]string{"VESSEL_X": "yes"},
				},
			},
		},
	}
	got, errs := resolveSandboxes(in)
	if errs.Aggregate() != nil {
		t.Fatalf("resolveSandboxes: %v", errs.Aggregate())
	}
	runner, ok := got["dev"]
	if !ok {
		t.Fatalf("missing runner for %q", "dev")
	}
	// echo the injected env var to prove WithDefaults layered
	// the EnvPolicy on top of the LocalRunner. We use sh -c
	// because the resolver does not set $PATH manipulation; the
	// runner inherits the host PATH on LocalRunner.
	out, err := runner.Exec(context.Background(), "sh", []string{"-c", "echo $VESSEL_X"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "yes" {
		t.Fatalf("VESSEL_X = %q; want %q", got, "yes")
	}
}

// TestResolveSandboxes_LocalRootDirConfines pins the rootDir →
// runner confinement contract: the LocalRunner returned for a
// Sandbox with rootDir=X must reject a per-call WorkDir that
// escapes X. If this regresses, the YAML "rootDir is the
// confinement boundary" promise silently weakens.
func TestResolveSandboxes_LocalRootDirConfines(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := t.TempDir() // sibling, not inside root
	if !filepath.IsAbs(outside) || strings.HasPrefix(outside, root) {
		t.Fatalf("test setup: outside %q must not be under root %q", outside, root)
	}
	in := map[string]v1alpha1.Sandbox{
		"dev": {
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindSandbox},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "dev"},
			Spec:       v1alpha1.SandboxSpec{Backend: "local", RootDir: root},
		},
	}
	got, errs := resolveSandboxes(in)
	if errs.Aggregate() != nil {
		t.Fatalf("resolveSandboxes: %v", errs.Aggregate())
	}
	if _, err := got["dev"].Exec(context.Background(), "true", nil, sandbox.ExecOptions{WorkDir: outside}); err == nil {
		t.Fatalf("Exec into %q outside root %q unexpectedly succeeded", outside, root)
	}
}

// TestResolveSandboxes_NsjailOnNonLinux confirms the resolver
// faithfully propagates the runner's "Linux-only" verdict instead
// of converting it into a generic Validation error. Operators on
// macOS should see the original classification.
func TestResolveSandboxes_NsjailOnNonLinux(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("non-linux only: this asserts the platform-gating path")
	}
	t.Parallel()
	in := map[string]v1alpha1.Sandbox{
		"prod": {
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindSandbox},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "prod"},
			Spec:       v1alpha1.SandboxSpec{Backend: "nsjail", RootDir: t.TempDir()},
		},
	}
	_, errs := resolveSandboxes(in)
	if !errdefs.IsNotAvailable(errs.Aggregate()) {
		t.Fatalf("expected NotAvailable on non-linux; got %v", errs.Aggregate())
	}
}

// TestResolveSandboxes_EmptyInput is the contract for "no
// sandboxes declared" — an empty map, never a nil. Downstream
// callers iterate the map without nil-guarding so a regression
// to nil would crash the fleet at startup.
func TestResolveSandboxes_EmptyInput(t *testing.T) {
	t.Parallel()
	got, errs := resolveSandboxes(nil)
	if errs.Aggregate() != nil {
		t.Fatalf("resolveSandboxes: %v", errs.Aggregate())
	}
	if got == nil {
		t.Fatalf("resolveSandboxes returned nil map; want empty non-nil")
	}
	if len(got) != 0 {
		t.Fatalf("len(got) = %d; want 0", len(got))
	}
}

// TestSandboxRunner_HonoursDefaultsFloor pins the WithDefaults
// integration: a per-call ExecOptions cannot widen the daemon-
// level Env policy. The decorator is meant as a security floor;
// if a regression makes per-call EnvPolicy *replace* the daemon
// floor we'd silently lose the sandbox's env discipline.
func TestSandboxRunner_HonoursDefaultsFloor(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Pre-populate a file the child should be able to read.
	if err := os.WriteFile(filepath.Join(root, "marker"), []byte("ok"), 0o600); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	in := map[string]v1alpha1.Sandbox{
		"dev": {
			TypeMeta:   v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindSandbox},
			ObjectMeta: v1alpha1.ObjectMeta{Name: "dev"},
			Spec: v1alpha1.SandboxSpec{
				Backend: "local",
				RootDir: root,
				Env:     &v1alpha1.SandboxEnv{Inject: map[string]string{"FLOOR": "1"}},
			},
		},
	}
	got, errs := resolveSandboxes(in)
	if errs.Aggregate() != nil {
		t.Fatalf("resolveSandboxes: %v", errs.Aggregate())
	}
	// Per-call ExecOptions sets a different env var; FLOOR
	// must still survive because the daemon-level Inject is a
	// floor, not a default-with-override.
	out, err := got["dev"].Exec(context.Background(), "sh",
		[]string{"-c", "echo $FLOOR-$EXTRA"},
		sandbox.ExecOptions{Env: sandbox.EnvPolicy{Inject: map[string]string{"EXTRA": "two"}}})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if got := strings.TrimSpace(string(out.Stdout)); got != "1-two" {
		t.Fatalf("env merge = %q; want %q (FLOOR must survive)", got, "1-two")
	}
}

func sandboxFixtureConfig(t *testing.T, body string) []apispec.Object {
	t.Helper()
	objs, err := apispec.DecodeAll(strings.NewReader(body), "<sandbox_test>")
	if err != nil {
		t.Fatalf("DecodeAll: %v", err)
	}
	return objs
}

func sandboxFixtureCatalog() *catalog.Catalog {
	cat := catalog.New()
	cat.RegisterEngine("noop-test", func(ref string, cfg map[string]any, deps catalog.Deps) (engine.Engine, error) {
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		}), nil
	})
	return cat
}

// TestResolve_AgentSandboxRef pins the end-to-end happy path:
// an Agent.spec.sandbox naming a Sandbox doc populates the
// resolved VesselPlan fields the fleet later reads.
func TestResolve_AgentSandboxRef(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfg := fmt.Sprintf(`
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata: {name: vesseld-default}
spec:
  control: {socket: /tmp/vesseld.sock}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Sandbox
metadata: {name: dev-shell}
spec:
  backend: local
  rootDir: %s
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata: {name: support}
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata: {name: helper}
spec:
  engine: {ref: noop-test}
  sandbox: dev-shell
`, root)
	plan, errs := Resolve(sandboxFixtureConfig(t, cfg), sandboxFixtureCatalog(), ResolveOptions{})
	if errs.Len() != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if _, ok := plan.SharedSandboxes["dev-shell"]; !ok {
		t.Fatalf("SharedSandboxes missing dev-shell; got %v", plan.SharedSandboxes)
	}
	if vp := plan.Vessels[0]; vp.SandboxName != "dev-shell" ||
		len(vp.SandboxAgents) != 1 || vp.SandboxAgents[0] != "helper" {
		t.Fatalf("VesselPlan sandbox fields = (%q, %v); want (dev-shell, [helper])",
			vp.SandboxName, vp.SandboxAgents)
	}
}

// TestResolve_RejectsUnknownSandboxRef confirms the NotFound
// classification for a dangling reference. Catching this at
// resolve time means the operator sees the failure at boot,
// not at first agent run.
func TestResolve_RejectsUnknownSandboxRef(t *testing.T) {
	t.Parallel()
	cfg := `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata: {name: vesseld-default}
spec:
  control: {socket: /tmp/vesseld.sock}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata: {name: support}
spec:
  agents: [helper]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata: {name: helper}
spec:
  engine: {ref: noop-test}
  sandbox: ghost
`
	_, errs := Resolve(sandboxFixtureConfig(t, cfg), sandboxFixtureCatalog(), ResolveOptions{})
	if errs.Len() == 0 {
		t.Fatal("expected NotFound for unknown sandbox ref")
	}
	if !errdefs.IsNotFound(errs.Aggregate()) {
		t.Fatalf("expected NotFound; got %v", errs.Aggregate())
	}
}

// TestResolve_RejectsMultiSandboxPerVessel pins the v0.2.0
// invariant. When two agents in the same Vessel reference two
// distinct Sandbox documents the resolver emits a Validation
// error so the fleet never has to disambiguate `exec` tool keys.
func TestResolve_RejectsMultiSandboxPerVessel(t *testing.T) {
	t.Parallel()
	root1 := t.TempDir()
	root2 := t.TempDir()
	cfg := fmt.Sprintf(`
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata: {name: vesseld-default}
spec:
  control: {socket: /tmp/vesseld.sock}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Sandbox
metadata: {name: dev}
spec: {backend: local, rootDir: %s}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Sandbox
metadata: {name: prod}
spec: {backend: local, rootDir: %s}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata: {name: support}
spec:
  agents: [a, b]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata: {name: a}
spec: {engine: {ref: noop-test}, sandbox: dev}
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata: {name: b}
spec: {engine: {ref: noop-test}, sandbox: prod}
`, root1, root2)
	_, errs := Resolve(sandboxFixtureConfig(t, cfg), sandboxFixtureCatalog(), ResolveOptions{})
	if !errdefs.IsValidation(errs.Aggregate()) {
		t.Fatalf("expected Validation error; got %v", errs.Aggregate())
	}
	if !strings.Contains(errs.Aggregate().Error(), "at most one Sandbox per Vessel") {
		t.Fatalf("error %q does not mention the v0.2.0 invariant", errs.Aggregate())
	}
}
