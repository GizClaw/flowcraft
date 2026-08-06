package config_test

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	sdkconfig "github.com/GizClaw/flowcraft/sdk/config"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/workspace"
	"github.com/GizClaw/flowcraft/sdk/workspace/config"
)

func TestParseStrictDocument(t *testing.T) {
	doc, err := config.Parse([]byte(`
version: v1
workspaces:
  project:
    driver: local
    settings:
      root: project
    scope:
      deny_read: [secret/**]
      allow_write: [output/**]
      mandatory_deny: [.git/**]
  scratch:
    driver: memory
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Version != config.VersionV1 {
		t.Fatalf("Version = %q, want %q", doc.Version, config.VersionV1)
	}
	if got := doc.Workspaces["project"].Driver; got != config.DriverLocal {
		t.Fatalf("project driver = %q, want %q", got, config.DriverLocal)
	}
	if doc.Workspaces["project"].Settings == nil {
		t.Fatal("project settings should remain opaque")
	}
	if got := doc.Workspaces["project"].Scope.AllowWrite; !reflect.DeepEqual(got, []string{"output/**"}) {
		t.Fatalf("allow_write = %v", got)
	}
}

func TestParseJSONDocument(t *testing.T) {
	doc, err := config.Parse([]byte(`{
		"version": "v1",
		"workspaces": {
			"scratch": {"driver": "memory"},
			"project": {"driver": "local", "settings": {"root": "data"}}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if doc.Version != config.VersionV1 {
		t.Fatalf("Version = %q, want %q", doc.Version, config.VersionV1)
	}
	if got := doc.Workspaces["project"].Driver; got != config.DriverLocal {
		t.Fatalf("project driver = %q, want %q", got, config.DriverLocal)
	}
}

func TestParseRejectsInvalidDocumentsAsValidation(t *testing.T) {
	tests := map[string]string{
		"unknown top-level field": "version: v1\nworkspaces: {}\nbogus: true\n",
		"unknown entry field":     "version: v1\nworkspaces:\n  x: {driver: memory, bogus: true}\n",
		"unknown scope field":     "version: v1\nworkspaces:\n  x:\n    driver: memory\n    scope: {bogus: []}\n",
		"trailing document":       "version: v1\nworkspaces: {}\n---\nversion: v1\nworkspaces: {}\n",
		"unsupported version":     "version: v2\nworkspaces: {}\n",
		"missing workspaces map":  "version: v1\n",
		"missing driver":          "version: v1\nworkspaces:\n  x: {}\n",
		"empty workspace name":    "version: v1\nworkspaces:\n  \"\": {driver: memory}\n",
		"duplicate workspace":     "version: v1\nworkspaces:\n  x: {driver: memory}\n  x: {driver: memory}\n",
		"invalid scope pattern":   "version: v1\nworkspaces:\n  x:\n    driver: memory\n    scope: {deny_read: ['[']}\n",
		"unknown json field":      `{"version":"v1","workspaces":{},"bogus":true}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := config.Parse([]byte(input))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !errdefs.IsValidation(err) {
				t.Fatalf("error = %v, want errdefs.Validation", err)
			}
		})
	}
}

func TestBuildLocalRelativeRootAndRegistryMetadata(t *testing.T) {
	base := t.TempDir()
	doc := mustParse(t, `
version: v1
workspaces:
  project:
    driver: local
    settings: {root: data/project}
    scope:
      allow_write: [output/**]
`)
	registry, err := config.NewBuilder(config.Deps{BaseDir: base}).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ws, ok := registry.Get("project")
	if !ok {
		t.Fatal("project workspace missing")
	}
	if _, ok := ws.(*workspace.ScopedWorkspace); !ok {
		t.Fatalf("workspace type = %T, want scoped", ws)
	}
	root, ok := registry.Root("project")
	if !ok {
		t.Fatal("local root metadata missing")
	}
	want, err := filepath.EvalSymlinks(filepath.Join(base, "data/project"))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if root != want {
		t.Fatalf("Root = %q, want %q", root, want)
	}
	if err := ws.Write(context.Background(), "output/result.txt", []byte("ok")); err != nil {
		t.Fatalf("scoped write: %v", err)
	}
	if err := ws.Write(context.Background(), "private/result.txt", []byte("no")); err == nil {
		t.Fatal("write outside allow_write succeeded")
	}
}

func TestBuildMemoryAndScopeEnforcement(t *testing.T) {
	doc := mustParse(t, `
version: v1
workspaces:
  scratch:
    driver: memory
    settings: {}
    scope:
      deny_read: [secret/**]
      allow_write: [public/**, secret/**]
      mandatory_deny: [secret/**]
`)
	registry, err := config.NewBuilder(config.Deps{}).Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	ws, ok := registry.Get("scratch")
	if !ok {
		t.Fatal("scratch workspace missing")
	}
	if _, ok := registry.Root("scratch"); ok {
		t.Fatal("memory workspace unexpectedly has root metadata")
	}
	if err := ws.Write(context.Background(), "public/file.txt", []byte("ok")); err != nil {
		t.Fatalf("allowed write: %v", err)
	}
	if err := ws.Write(context.Background(), "private/file.txt", []byte("no")); err == nil {
		t.Fatal("write outside allow list succeeded")
	}
	if err := ws.Write(context.Background(), "secret/file.txt", []byte("no")); err == nil {
		t.Fatal("mandatory deny did not override allow_write")
	}
}

func TestBuildRejectsUnknownFieldsDriverAndNilAsValidation(t *testing.T) {
	tests := []struct {
		name string
		yaml string
	}{
		{"unknown driver", "version: v1\nworkspaces:\n  x: {driver: missing}\n"},
		{"memory unknown setting", "version: v1\nworkspaces:\n  x:\n    driver: memory\n    settings: {unexpected: true}\n"},
		{"local missing root", "version: v1\nworkspaces:\n  x: {driver: local}\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := config.NewBuilder(config.Deps{}).Build(context.Background(), mustParse(t, tc.yaml))
			if err == nil || !errdefs.IsValidation(err) {
				t.Fatalf("Build error = %v, want errdefs.Validation", err)
			}
		})
	}

	builder := config.NewBuilder(config.Deps{})
	if err := builder.RegisterFactory("nil", nil); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("nil factory error = %v, want errdefs.Validation", err)
	}
	if err := builder.RegisterFactory(config.DriverMemory, func(context.Context, sdkconfig.Input) (config.Resource, error) {
		return config.Resource{}, nil
	}); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("duplicate driver error = %v, want errdefs.Validation", err)
	}

	nilResourceCloses := 0
	if err := builder.RegisterFactory("nil-resource", func(context.Context, sdkconfig.Input) (config.Resource, error) {
		return config.Resource{Close: func() error {
			nilResourceCloses++
			return nil
		}}, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	doc := mustParse(t, "version: v1\nworkspaces:\n  x: {driver: nil-resource}\n")
	if _, err := builder.Build(context.Background(), doc); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("nil resource error = %v, want errdefs.Validation", err)
	}
	if nilResourceCloses != 1 {
		t.Fatalf("nil resource closes = %d, want 1", nilResourceCloses)
	}
}

func TestCustomFactoryAndImmutableRegistry(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	custom := workspace.NewMemWorkspace()
	if err := builder.RegisterFactory("custom", func(_ context.Context, in sdkconfig.Input) (config.Resource, error) {
		type customSettings struct {
			Root string `json:"root"`
		}
		var s customSettings
		if err := in.Settings.Decode(&s); err != nil {
			return config.Resource{}, err
		}
		return config.Resource{Workspace: custom, Root: s.Root}, nil
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	doc := mustParse(t, `
version: v1
workspaces:
  zed:
    driver: custom
    settings: {root: /host/custom}
  alpha:
    driver: memory
`)
	registry, err := builder.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"alpha", "zed"}) {
		t.Fatalf("Names = %v, want [alpha zed]", got)
	}
	names := registry.Names()
	names[0] = "mutated"
	if got := registry.Names(); !reflect.DeepEqual(got, []string{"alpha", "zed"}) {
		t.Fatalf("Names after caller mutation = %v", got)
	}
	if got, ok := registry.Get("zed"); !ok || got != custom {
		t.Fatalf("Get(zed) = (%T, %v), want custom", got, ok)
	}
	if root, ok := registry.Root("zed"); !ok || root != "/host/custom" {
		t.Fatalf("Root(zed) = (%q, %v)", root, ok)
	}
}

func TestFactoryErrorsAreValidation(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	if err := builder.RegisterFactory("broken", func(context.Context, sdkconfig.Input) (config.Resource, error) {
		return config.Resource{}, errors.New("broken")
	}); err != nil {
		t.Fatalf("RegisterFactory: %v", err)
	}
	doc := mustParse(t, "version: v1\nworkspaces:\n  x: {driver: broken}\n")
	if _, err := builder.Build(context.Background(), doc); err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want errdefs.Validation", err)
	}
}

func TestFactoryPreservesClassifiedErrors(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	if err := builder.RegisterFactory("unavailable", func(context.Context, sdkconfig.Input) (config.Resource, error) {
		return config.Resource{}, errdefs.NotAvailablef("backend offline")
	}); err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, "version: v1\nworkspaces:\n  x: {driver: unavailable}\n")
	if _, err := builder.Build(context.Background(), doc); !errdefs.IsNotAvailable(err) {
		t.Fatalf("Build error = %v, want preserved NotAvailable", err)
	}
}

func TestFactoryClassifiesContextCancellation(t *testing.T) {
	builder := config.NewBuilder(config.Deps{})
	if err := builder.RegisterFactory("cancelled", func(context.Context, sdkconfig.Input) (config.Resource, error) {
		return config.Resource{}, context.Canceled
	}); err != nil {
		t.Fatal(err)
	}
	doc := mustParse(t, "version: v1\nworkspaces:\n  x: {driver: cancelled}\n")
	if _, err := builder.Build(context.Background(), doc); !errdefs.IsAborted(err) {
		t.Fatalf("Build error = %v, want Aborted", err)
	}
}

type closeableWorkspace struct {
	workspace.Workspace
	closes int
}

func (w *closeableWorkspace) Close() error {
	w.closes++
	return nil
}

func TestRegistryOwnsFactoryWorkspaceLifecycle(t *testing.T) {
	for _, failAfterBuild := range []bool{false, true} {
		t.Run(map[bool]string{false: "registry close", true: "partial failure"}[failAfterBuild], func(t *testing.T) {
			resource := &closeableWorkspace{Workspace: workspace.NewMemWorkspace()}
			builder := config.NewBuilder(config.Deps{})
			if err := builder.RegisterFactory("closeable", func(context.Context, sdkconfig.Input) (config.Resource, error) {
				return config.Resource{Workspace: resource}, nil
			}); err != nil {
				t.Fatal(err)
			}
			input := "version: v1\nworkspaces:\n  a:\n    driver: closeable\n    scope: {allow_write: ['**']}\n"
			if failAfterBuild {
				input += "  z: {driver: missing}\n"
			}
			registry, err := builder.Build(context.Background(), mustParse(t, input))
			if failAfterBuild {
				if err == nil || resource.closes != 1 {
					t.Fatalf("Build = (%v, %v), closes=%d; want error and one close",
						registry, err, resource.closes)
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
			if resource.closes != 1 {
				t.Fatalf("closes = %d, want 1", resource.closes)
			}
		})
	}
}

func TestRegistryResolveSourceAdapter(t *testing.T) {
	registry, err := config.NewBuilder(config.Deps{}).Build(
		context.Background(),
		mustParse(t, "version: v1\nworkspaces:\n  scratch: {driver: memory}\n"),
	)
	if err != nil {
		t.Fatal(err)
	}
	value, err := registry.Resolve(context.Background(), "scratch")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := value.(workspace.Workspace); !ok {
		t.Fatalf("Resolve returned %T, want workspace.Workspace", value)
	}
	if _, err := registry.Resolve(context.Background(), "missing"); !errdefs.IsValidation(err) {
		t.Fatalf("missing Resolve error = %v, want Validation", err)
	}
}

func mustParse(t *testing.T, input string) config.Document {
	t.Helper()
	doc, err := config.Parse([]byte(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}
