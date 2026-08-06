package config_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/sandbox"
	sandboxconfig "github.com/GizClaw/flowcraft/sdk/sandbox/config"
	workspaceconfig "github.com/GizClaw/flowcraft/sdk/workspace/config"
)

func TestParseStrictDocument(t *testing.T) {
	doc := mustParse(t, `
version: v1
sandboxes:
  build:
    backend: local
    workspace: project
    settings:
      default_max_output_bytes: 4096
    defaults:
      timeout: 30s
      env:
        allow: [PATH]
        inject: {CI: "true"}
      net:
        mode: default
      resources:
        cpu_millicores: 500
        memory_bytes: 1048576
        max_output_bytes: 2048
    allowed_commands: [go]
    approval:
      outside_workdir: true
      non_default_network: true
      sensitive_commands: [rm, "git"]
`)
	entry := doc.Sandboxes["build"]
	if doc.Version != sandboxconfig.VersionV1 || entry.Backend != sandboxconfig.BackendLocal {
		t.Fatalf("parsed document = %#v", doc)
	}
	if entry.Settings == nil {
		t.Fatal("backend settings should remain opaque")
	}
	if got := time.Duration(entry.Defaults.Timeout); got != 30*time.Second {
		t.Fatalf("timeout = %v", got)
	}
	if got := entry.Defaults.Net.Mode; got != sandboxconfig.NetModeDefault {
		t.Fatalf("net mode = %q", got)
	}
}

func TestParseRejectsStrictVersionAndTrailingDocuments(t *testing.T) {
	tests := map[string]string{
		"unknown top-level":   "version: v1\nsandboxes: {}\nbogus: true\n",
		"unknown entry":       "version: v1\nsandboxes:\n  x: {backend: local, workspace: w, bogus: true}\n",
		"unknown defaults":    "version: v1\nsandboxes:\n  x: {backend: local, workspace: w, defaults: {workdir: tmp}}\n",
		"unknown approval":    "version: v1\nsandboxes:\n  x: {backend: local, workspace: w, approval: {bogus: true}}\n",
		"unsupported version": "version: v2\nsandboxes: {}\n",
		"missing map":         "version: v1\n",
		"trailing document":   "version: v1\nsandboxes: {}\n---\nversion: v1\nsandboxes: {}\n",
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := sandboxconfig.Parse([]byte(input))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Parse error = %v, want validation", err)
			}
		})
	}
}

func TestParseValidatesDurationNetworkAndResources(t *testing.T) {
	tests := map[string]string{
		"numeric duration":      "timeout: 30",
		"invalid duration":      "timeout: forever",
		"negative duration":     "timeout: -1s",
		"unknown net mode":      "net: {mode: open}",
		"allow list empty":      "net: {mode: allow_list, allow_hosts: []}",
		"allow list with proxy": "net: {mode: allow_list, allow_hosts: [example.com], proxy: http://proxy}",
		"proxy missing":         "net: {mode: proxy}",
		"default with hosts":    "net: {mode: default, allow_hosts: [example.com]}",
		"negative cpu":          "resources: {cpu_millicores: -1}",
		"negative memory":       "resources: {memory_bytes: -1}",
		"negative disk":         "resources: {disk_bytes: -1}",
		"negative output":       "resources: {max_output_bytes: -1}",
		"cpu without timeout":   "resources: {cpu_millicores: 100}",
	}
	for name, defaults := range tests {
		t.Run(name, func(t *testing.T) {
			input := "version: v1\nsandboxes:\n  x:\n    backend: local\n    workspace: w\n    defaults: {" + defaults + "}\n"
			_, err := sandboxconfig.Parse([]byte(input))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Parse error = %v, want validation", err)
			}
		})
	}
}

func TestBuildLocalWithRealWorkspace(t *testing.T) {
	root := t.TempDir()
	workspaces := buildWorkspaces(t, root, false)
	doc := mustParse(t, `
version: v1
sandboxes:
  build:
    backend: local
    workspace: project
    settings: {default_max_output_bytes: 1024}
    defaults:
      timeout: 2s
      env: {allow: [], inject: {FLOWCRAFT_TEST: "yes"}}
      net: {mode: default}
      resources: {max_output_bytes: 1024}
    allowed_commands: [sh]
`)
	registry, err := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: workspaces}).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runner, ok := registry.Get("build")
	if !ok {
		t.Fatal("build sandbox missing")
	}
	result, err := runner.Exec(context.Background(), "sh", []string{"-c", `printf %s "$FLOWCRAFT_TEST"`}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.Stdout != "yes" {
		t.Fatalf("stdout = %q, want yes", result.Stdout)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"build"}) {
		t.Fatalf("Names = %v", got)
	}
}

func TestBuildRejectsWorkspaceBackendAndSettingsFailures(t *testing.T) {
	root := t.TempDir()
	workspaces := buildWorkspaces(t, root, true)
	tests := []struct {
		name  string
		entry string
		want  string
	}{
		{"unknown workspace", "{backend: local, workspace: missing}", "unknown workspace"},
		{"rootless memory", "{backend: local, workspace: memory}", "host root"},
		{"unknown backend", "{backend: missing, workspace: project}", "unknown backend"},
		{"local unknown setting", "{backend: local, workspace: project, settings: {bogus: true}}", "bogus"},
		{"local negative output", "{backend: local, workspace: project, settings: {default_max_output_bytes: -1}}", "non-negative"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := mustParse(t, "version: v1\nsandboxes:\n  x: "+tc.entry+"\n")
			_, err := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: workspaces}).Build(context.Background(), doc)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Build error = %v, want containing %q", err, tc.want)
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want validation", err)
			}
		})
	}
}

type captureRunner struct {
	calls int
	opts  sandbox.ExecOptions
}

type closeableRunner struct {
	captureRunner
	closes int
}

func (r *closeableRunner) Close() error {
	r.closes++
	return nil
}

func (r *captureRunner) Enforcement() sandbox.Enforcement {
	return sandbox.Enforcement{
		EnvAllowList: true,
		NetModes:     []sandbox.NetMode{sandbox.NetDenyAll},
		MemoryCap:    true,
		CPUCap:       true,
	}
}

func (r *captureRunner) Exec(_ context.Context, _ string, _ []string, opts sandbox.ExecOptions) (*sandbox.ExecResult, error) {
	r.calls++
	r.opts = opts
	return &sandbox.ExecResult{}, nil
}

func TestCustomFactoryReceivesRootAndOpaqueSettings(t *testing.T) {
	root := t.TempDir()
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: buildWorkspaces(t, root, false)})
	captured := struct {
		root  string
		label string
	}{}
	runner := &captureRunner{}
	err := builder.RegisterFactory("custom", func(_ context.Context, input sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		type settings struct {
			Label string `json:"label"`
		}
		var value settings
		if err := input.Settings.Decode(&value); err != nil {
			return nil, err
		}
		captured.root, captured.label = input.Root, value.Label
		return runner, nil
	})
	if err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	doc := mustParse(t, "version: v1\nsandboxes:\n  x: {backend: custom, workspace: project, settings: {label: ok}}\n")
	registry, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	wantRoot, _ := filepath.EvalSymlinks(root)
	if captured.root != wantRoot || captured.label != "ok" {
		t.Fatalf("factory input = %#v, want root %q label ok", captured, wantRoot)
	}
	if got, ok := registry.Get("x"); !ok || got == nil {
		t.Fatal("custom runner missing")
	}
}

func TestBuildApprovalRequiresDependency(t *testing.T) {
	root := t.TempDir()
	workspaces := buildWorkspaces(t, root, false)
	for name, approval := range map[string]string{
		"outside workdir":    "{outside_workdir: true}",
		"network":            "{non_default_network: true}",
		"sensitive commands": "{sensitive_commands: [rm]}",
	} {
		t.Run(name, func(t *testing.T) {
			doc := mustParse(t, "version: v1\nsandboxes:\n  x:\n    backend: local\n    workspace: project\n    approval: "+approval+"\n")
			_, err := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: workspaces}).Build(context.Background(), doc)
			if err == nil || !errdefs.IsValidation(err) || !strings.Contains(err.Error(), "Approver") {
				t.Fatalf("Build error = %v, want missing Approver validation", err)
			}
		})
	}
}

func TestApprovalSeesEffectiveDefaults(t *testing.T) {
	root := t.TempDir()
	var approved sandbox.ApprovalRequest
	approver := func(_ context.Context, req sandbox.ApprovalRequest) (sandbox.Decision, error) {
		approved = req
		return sandbox.Allow, nil
	}
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, root, false),
		Approver:   approver,
	})
	inner := &captureRunner{}
	if err := builder.RegisterFactory("capture", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return inner, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	doc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: capture
    workspace: project
    defaults:
      timeout: 3s
      net: {mode: deny_all}
      resources: {memory_bytes: 99}
    approval: {non_default_network: true}
`)
	registry, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	runner, _ := registry.Get("x")
	if _, err := runner.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{Timeout: 10 * time.Second}); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if approved.Exec.Opts.Timeout != 3*time.Second ||
		approved.Exec.Opts.Net.Mode != sandbox.NetDenyAll ||
		approved.Exec.Opts.Resources.MemoryBytes != 99 {
		t.Fatalf("approver saw opts = %#v", approved.Exec.Opts)
	}
	if inner.opts.Net.Mode != sandbox.NetDenyAll {
		t.Fatalf("backend saw opts = %#v", inner.opts)
	}
}

func TestAllowedCommandsNilVersusEmpty(t *testing.T) {
	root := t.TempDir()
	workspaces := buildWorkspaces(t, root, false)
	for name, tc := range map[string]struct {
		allowed    string
		wantDenied bool
	}{
		"nil":   {"", false},
		"empty": {"    allowed_commands: []\n", true},
	} {
		t.Run(name, func(t *testing.T) {
			builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: workspaces})
			inner := &captureRunner{}
			if err := builder.RegisterFactory("capture", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
				return inner, nil
			}); err != nil {
				t.Fatal(err)
			}
			doc := mustParse(t, "version: v1\nsandboxes:\n  x:\n    backend: capture\n    workspace: project\n"+tc.allowed)
			registry, err := builder.Build(context.Background(), doc)
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			runner, _ := registry.Get("x")
			_, err = runner.Exec(context.Background(), "echo", nil, sandbox.ExecOptions{})
			if tc.wantDenied != errdefs.IsPolicyDenied(err) {
				t.Fatalf("Exec error = %v, want denied=%v", err, tc.wantDenied)
			}
			if tc.wantDenied && inner.calls != 0 {
				t.Fatal("empty allow-list reached backend")
			}
		})
	}
}

func TestBuildRejectsPoliciesBackendCannotEnforce(t *testing.T) {
	workspaces := buildWorkspaces(t, t.TempDir(), false)
	for name, defaults := range map[string]string{
		"network": "net: {mode: deny_all}",
		"disk":    "resources: {disk_bytes: 1024}",
	} {
		t.Run(name, func(t *testing.T) {
			doc := mustParse(t, "version: v1\nsandboxes:\n  x:\n    backend: local\n    workspace: project\n    defaults:\n      "+defaults+"\n")
			_, err := sandboxconfig.NewBuilder(sandboxconfig.Deps{
				Workspaces: workspaces,
			}).Build(context.Background(), doc)
			if !errdefs.IsNotAvailable(err) {
				t.Fatalf("Build error = %v, want NotAvailable", err)
			}
		})
	}

	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: workspaces})
	if err := builder.RegisterFactory("opaque", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return sandbox.NoopRunner{}, nil
	}); err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, `
version: v1
sandboxes:
  x:
    backend: opaque
    workspace: project
    defaults:
      env: {allow: []}
`)
	if _, err := builder.Build(context.Background(), doc); !errdefs.IsNotAvailable(err) {
		t.Fatalf("unreported env enforcement error = %v, want NotAvailable", err)
	}
}

func TestRegistryResolveSourceAdapter(t *testing.T) {
	registry, err := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, t.TempDir(), false),
	}).Build(context.Background(), mustParse(t, `
version: v1
sandboxes:
  local: {backend: local, workspace: project}
`))
	if err != nil {
		t.Fatal(err)
	}
	value, err := registry.Resolve(context.Background(), "local")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.(sandbox.Runner); !ok {
		t.Fatalf("Resolve returned %T, want sandbox.Runner", value)
	}
	if _, err := registry.Resolve(context.Background(), "missing"); !errdefs.IsValidation(err) {
		t.Fatalf("missing Resolve error = %v, want Validation", err)
	}
}

func TestFactoryClassifiesContextCancellation(t *testing.T) {
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
		Workspaces: buildWorkspaces(t, t.TempDir(), false),
	})
	if err := builder.RegisterFactory("cancelled", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return nil, context.DeadlineExceeded
	}); err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, "version: v1\nsandboxes:\n  x: {backend: cancelled, workspace: project}\n")
	if _, err := builder.Build(context.Background(), doc); !errdefs.IsTimeout(err) {
		t.Fatalf("Build error = %v, want Timeout", err)
	}
}

func TestRegistryOwnsFactoryRunnerLifecycle(t *testing.T) {
	for _, failAfterBuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "registry close", true: "partial failure"}[failAfterBuild], func(t *testing.T) {
			runner := &closeableRunner{}
			builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{
				Workspaces: buildWorkspaces(t, t.TempDir(), false),
			})
			if err := builder.RegisterFactory("closeable", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
				return runner, nil
			}); err != nil {
				t.Fatal(err)
			}
			input := "version: v1\nsandboxes:\n  a: {backend: closeable, workspace: project}\n"
			if failAfterBuild {
				input += "  z: {backend: missing, workspace: project}\n"
			}
			registry, err := builder.Build(context.Background(), mustParse(t, input))
			if failAfterBuild {
				if err == nil || runner.closes != 1 {
					t.Fatalf("Build = (%v, %v), closes=%d; want error and one close",
						registry, err, runner.closes)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := registry.Close(); err != nil {
				t.Fatal(err)
			}
			if err := registry.Close(); err != nil {
				t.Fatal(err)
			}
			if runner.closes != 1 {
				t.Fatalf("closes = %d, want 1", runner.closes)
			}
		})
	}
}

func TestRegistryImmutableAndBuilderValidation(t *testing.T) {
	root := t.TempDir()
	builder := sandboxconfig.NewBuilder(sandboxconfig.Deps{Workspaces: buildWorkspaces(t, root, false)})
	if err := builder.RegisterFactory("", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return sandbox.NoopRunner{}, nil
	}); err == nil {
		t.Fatal("empty backend registration succeeded")
	}
	if err := builder.RegisterFactory("nil", nil); err == nil {
		t.Fatal("nil factory registration succeeded")
	}
	if err := builder.RegisterFactory(sandboxconfig.BackendLocal, func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return sandbox.NoopRunner{}, nil
	}); err == nil {
		t.Fatal("duplicate backend registration succeeded")
	}
	if err := builder.RegisterFactory("broken", func(context.Context, sandboxconfig.FactoryInput) (sandbox.Runner, error) {
		return nil, errors.New("broken")
	}); err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, `
version: v1
sandboxes:
  zed: {backend: local, workspace: project}
  alpha: {backend: local, workspace: project}
`)
	registry, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"alpha", "zed"}) {
		t.Fatalf("Names = %v", got)
	}
	names := registry.Names()
	names[0] = "mutated"
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"alpha", "zed"}) {
		t.Fatalf("Names after mutation = %v", got)
	}
}

func buildWorkspaces(t *testing.T, root string, memory bool) *workspaceconfig.Registry {
	t.Helper()
	input := "version: v1\nworkspaces:\n  project:\n    driver: local\n    settings:\n      root: " + root + "\n"
	if memory {
		input += "  memory:\n    driver: memory\n"
	}
	doc, err := workspaceconfig.Parse([]byte(input))
	if err != nil {
		t.Fatalf("workspace Parse: %v", err)
	}
	registry, err := workspaceconfig.NewBuilder(workspaceconfig.Deps{}).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("workspace Build: %v", err)
	}
	return registry
}

func mustParse(t *testing.T, input string) sandboxconfig.Document {
	t.Helper()
	doc, err := sandboxconfig.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}
