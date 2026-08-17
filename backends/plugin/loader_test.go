package plugin

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

func writePluginDir(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, "plugin.yaml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeLayerFile(t *testing.T, root, name, rel, body string) {
	t.Helper()
	path := filepath.Join(root, name, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadCollectsAndMergesLayers(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	writePluginDir(t, root, "acme.alpha", `
name: acme.alpha
version: 1.0.0
artifacts:
  - type: layer
    path: layers/10.yaml
    priority: 10
`)
	writeLayerFile(t, root, "acme.alpha", "layers/10.yaml", `
resources:
  alpha:
    kind: inference.Provider
    impl: acme.alpha
    settings: {}
`)

	writePluginDir(t, root, "acme.beta", `
name: acme.beta
version: 1.0.0
artifacts:
  - type: layer
    path: layers/20.yaml
    priority: 20
`)
	writeLayerFile(t, root, "acme.beta", "layers/20.yaml", `
resources:
  beta:
    kind: inference.Provider
    impl: acme.beta
    settings: {}
`)

	set, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha", "acme.beta"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(set.Plugins))
	}
	if len(set.Layers) != 2 {
		t.Fatalf("layers = %d, want 2", len(set.Layers))
	}
	if set.Layers[0].Name != "acme.alpha" || set.Layers[1].Name != "acme.beta" {
		t.Fatalf("layer order = %s,%s; want alpha,beta",
			set.Layers[0].Name, set.Layers[1].Name)
	}

	base := deploy.Layer{
		Priority: 0,
		Name:     "base",
		Source:   resource.Source{Inline: []byte(`{"version":"1","resources":{}}`)},
	}
	merged, _, err := deploy.LoadLayers(
		ctx, append([]deploy.Layer{base}, set.Layers...))
	if err != nil {
		t.Fatalf("LoadLayers: %v", err)
	}
	if _, ok := merged.Resources["alpha"]; !ok {
		t.Fatal("merged document missing alpha resource")
	}
	if _, ok := merged.Resources["beta"]; !ok {
		t.Fatal("merged document missing beta resource")
	}
}

func TestLoadRejectsProvidedConflicts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.one", `
name: acme.one
version: 1.0.0
provides:
  - kind: inference.Provider
    impl: acme.provider
`)
	writePluginDir(t, root, "acme.two", `
name: acme.two
version: 1.0.0
provides:
  - kind: inference.Provider
    impl: acme.provider
`)

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.one", "acme.two"},
	})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
}

type stubFactory struct {
	spec resource.Spec
}

func (f stubFactory) Spec() resource.Spec { return f.spec }

func (f stubFactory) New(context.Context, resource.Input) (any, error) {
	return nil, nil
}

func TestLoadRejectsConflictWithRegistry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.one", `
name: acme.one
version: 1.0.0
provides:
  - kind: inference.Provider
    impl: acme.provider
`)

	target := NewTarget()
	if err := target.Resources.Register(stubFactory{
		spec: resource.Spec{Kind: "inference.Provider", Impl: "acme.provider"},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	_, err := NewLoader(WithTarget(target)).Load(
		ctx, PluginsConfig{Dirs: []string{root}, Enabled: []string{"acme.one"}})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict", err)
	}
}

func TestLoadDependencies(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.base", `
name: acme.base
version: 1.2.0
`)
	writePluginDir(t, root, "acme.app", `
name: acme.app
version: 1.0.0
requires:
  plugins:
    - acme.base@^1.0.0
`)

	set, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.base", "acme.app"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(set.Plugins))
	}
}

func TestLoadMissingDependency(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.app", `
name: acme.app
version: 1.0.0
requires:
  plugins:
    - acme.missing@^1.0.0
`)

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.app"},
	})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestLoadDependencyVersionMismatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.base", `
name: acme.base
version: 0.9.0
`)
	writePluginDir(t, root, "acme.app", `
name: acme.app
version: 1.0.0
requires:
  plugins:
    - acme.base@^1.0.0
`)

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.base", "acme.app"},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation", err)
	}
}

func TestLoadDependencyCycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.a", `
name: acme.a
version: 1.0.0
requires:
  plugins:
    - acme.b
`)
	writePluginDir(t, root, "acme.b", `
name: acme.b
version: 1.0.0
requires:
  plugins:
    - acme.a
`)

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.a", "acme.b"},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for cycle", err)
	}
}

func TestLoadRequiresCore(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.old", `
name: acme.old
version: 1.0.0
requires:
  core: ">=0.4.0"
`)
	writePluginDir(t, root, "acme.new", `
name: acme.new
version: 1.0.0
requires:
  core: ">=0.5.0"
`)

	loader := NewLoader(WithCoreVersion("0.4.0"))
	_, err := loader.Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.old", "acme.new"},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for requires.core", err)
	}

	// Only the plugin that is satisfied loads when the whitelist is
	// explicit; the unsatisfied one is never activated.
	loader = NewLoader(WithCoreVersion("0.4.0"))
	set, err := loader.Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.old"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 1 || set.Plugins[0].Manifest().Name != "acme.old" {
		t.Fatalf("plugins = %+v, want only acme.old", set.Plugins)
	}
}

func TestLoadEnabledFilter(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.2.0\n")
	writePluginDir(t, root, "acme.beta", "name: acme.beta\nversion: 0.9.0\n")

	set, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha@^1.0.0"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 1 || set.Plugins[0].Manifest().Name != "acme.alpha" {
		t.Fatalf("plugins = %+v, want only acme.alpha", set.Plugins)
	}
}

func TestLoadEnabledNameNotFound(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.typo"},
	})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound for a whitelist name no plugin matches", err)
	}
}

func TestLoadEnabledVersionMismatchExcludes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	// A discovered plugin failing the whitelist's version constraint is
	// the "waiting for upgrade" state: it stays disabled without error,
	// so Reconcile can activate it once the version satisfies.
	set, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha@^2.0.0"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 0 {
		t.Fatalf("plugins = %d, want 0 for unsatisfied whitelist version", len(set.Plugins))
	}
}

func TestLoadDuplicateName(t *testing.T) {
	ctx := context.Background()
	rootA := t.TempDir()
	rootB := t.TempDir()
	writePluginDir(t, rootA, "acme.dup", "name: acme.dup\nversion: 1.0.0\n")
	writePluginDir(t, rootB, "acme.dup", "name: acme.dup\nversion: 1.0.0\n")

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{rootA, rootB},
		Enabled: []string{"acme.dup"},
	})
	if err == nil || !errdefs.IsConflict(err) {
		t.Fatalf("err = %v, want Conflict for duplicate name", err)
	}
}

func TestLoadInvalidLayer(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()

	writePluginDir(t, root, "acme.bad", `
name: acme.bad
version: 1.0.0
artifacts:
  - type: layer
    path: layers/10.yaml
`)
	writeLayerFile(t, root, "acme.bad", "layers/10.yaml", `
resources:
  x:
    kind: inference.Provider
    impl: acme.bad
    settings: {}
bogus_top_level: 1
`)

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.bad"},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for bad layer", err)
	}
}

func TestLoadMissingLayerFile(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.bad", `
name: acme.bad
version: 1.0.0
artifacts:
  - type: layer
    path: layers/10.yaml
`)

	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.bad"},
	})
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation for missing layer", err)
	}
}

func TestLoadMissingRootDir(t *testing.T) {
	ctx := context.Background()
	_, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{filepath.Join(t.TempDir(), "nope")},
		Enabled: []string{"acme.x"},
	})
	if err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("err = %v, want NotFound", err)
	}
}

func TestSetCloseAndApply(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	set, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := set.Apply(ctx, NewTarget()); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if err := set.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestReconcileChanges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")
	writePluginDir(t, root, "acme.beta", "name: acme.beta\nversion: 1.0.0\n")
	writePluginDir(t, root, "acme.gamma", "name: acme.gamma\nversion: 1.0.0\n")

	loader := NewLoader()
	set1, err := loader.Load(ctx, PluginsConfig{
		Dirs: []string{root},
		Enabled: []string{
			"acme.alpha",
			"acme.beta@^1.0.0",
			"acme.gamma@^2.0.0", // discovered but not yet matching
		},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set1.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(set1.Plugins))
	}

	// Version transitions drive the diff: alpha changes, beta falls out
	// of its constraint (Removed), gamma satisfies its constraint
	// (Added).
	if err := os.WriteFile(
		filepath.Join(root, "acme.alpha", "plugin.yaml"),
		[]byte("name: acme.alpha\nversion: 1.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "acme.beta", "plugin.yaml"),
		[]byte("name: acme.beta\nversion: 0.9.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "acme.gamma", "plugin.yaml"),
		[]byte("name: acme.gamma\nversion: 2.0.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	set2, changes, err := loader.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(changes.Added, []string{"acme.gamma"}) {
		t.Fatalf("added = %v", changes.Added)
	}
	if !slices.Equal(changes.Removed, []string{"acme.beta"}) {
		t.Fatalf("removed = %v", changes.Removed)
	}
	if !slices.Equal(changes.Changed, []string{"acme.alpha"}) {
		t.Fatalf("changed = %v", changes.Changed)
	}
	if set2 == set1 {
		t.Fatal("expected a fresh projection after changes")
	}
	if len(set2.Plugins) != 2 {
		t.Fatalf("plugins = %d, want 2", len(set2.Plugins))
	}
}

func TestReconcileEnabledNameDisappears(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	loader := NewLoader()
	set1, err := loader.Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// An enabled plugin whose directory disappears is a broken
	// deployment: Reconcile fails loud and keeps the previous
	// projection instead of reporting Removed.
	if err := os.Remove(filepath.Join(root, "acme.alpha", "plugin.yaml")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loader.Reconcile(ctx); err == nil || !errdefs.IsNotFound(err) {
		t.Fatalf("Reconcile = %v, want NotFound", err)
	}
	if loader.lastSet != set1 {
		t.Fatal("failed Reconcile must retain the previous projection")
	}
}

func TestReconcileLayerContentChange(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", `
name: acme.alpha
version: 1.0.0
artifacts:
  - type: layer
    path: layers/10.yaml
    priority: 10
`)
	writeLayerFile(t, root, "acme.alpha", "layers/10.yaml", `
resources:
  alpha:
    kind: inference.Provider
    impl: acme.alpha
    settings: {}
`)

	loader := NewLoader()
	set1, err := loader.Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Same manifest, different layer content.
	writeLayerFile(t, root, "acme.alpha", "layers/10.yaml", `
resources:
  alpha:
    kind: inference.Provider
    impl: acme.alpha
    settings:
      model: gpt-5
`)
	set2, changes, err := loader.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if !slices.Equal(changes.Changed, []string{"acme.alpha"}) {
		t.Fatalf("changed = %v", changes.Changed)
	}
	if pluginFingerprint(set1.Plugins[0]) == pluginFingerprint(set2.Plugins[0]) {
		t.Fatal("layer content change must alter the plugin fingerprint")
	}
}

func TestReconcileNoChangesReturnsSameSet(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	loader := NewLoader()
	set1, err := loader.Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	set2, changes, err := loader.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if changes.Any() {
		t.Fatalf("changes = %+v, want none", changes)
	}
	if set2 != set1 {
		t.Fatal("no changes must return the previous projection")
	}
}

func TestReconcileFailureKeepsProjection(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	loader := NewLoader()
	set1, err := loader.Load(ctx, PluginsConfig{
		Dirs:    []string{root},
		Enabled: []string{"acme.alpha"},
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(root, "acme.alpha", "plugin.yaml"),
		[]byte("name: acme.alpha\nversion: bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set2, changes, err := loader.Reconcile(ctx)
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("Reconcile err = %v, want Validation", err)
	}
	if set2 != nil || changes.Any() {
		t.Fatalf("failed Reconcile must return nil set and no changes, got %v %+v", set2, changes)
	}
	if loader.lastSet != set1 {
		t.Fatal("failed Reconcile must retain the previous projection")
	}
	if len(set1.Plugins) != 1 {
		t.Fatalf("previous projection damaged: plugins = %d", len(set1.Plugins))
	}

	// Recovery: after fixing the manifest, Reconcile reflects the
	// change and the state is not corrupted by the failed attempt.
	if err := os.WriteFile(
		filepath.Join(root, "acme.alpha", "plugin.yaml"),
		[]byte("name: acme.alpha\nversion: 1.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	set3, changes, err := loader.Reconcile(ctx)
	if err != nil {
		t.Fatalf("Reconcile after fix: %v", err)
	}
	if !slices.Equal(changes.Changed, []string{"acme.alpha"}) {
		t.Fatalf("changed = %v", changes.Changed)
	}
	if set3 == set1 {
		t.Fatal("expected a fresh projection after recovery")
	}
}

func TestReconcileBeforeLoad(t *testing.T) {
	_, _, err := NewLoader().Reconcile(context.Background())
	if err == nil || !errdefs.IsValidation(err) {
		t.Fatalf("err = %v, want Validation", err)
	}
}

func TestLoadDisabledByDefault(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writePluginDir(t, root, "acme.alpha", "name: acme.alpha\nversion: 1.0.0\n")

	// An absent whitelist enables nothing: even a broken plugin
	// directory must not fail the load.
	set, err := NewLoader().Load(ctx, PluginsConfig{Dirs: []string{root}})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(set.Plugins) != 0 {
		t.Fatalf("plugins = %d, want 0 by default", len(set.Plugins))
	}

	// A missing root directory is also tolerated: nothing is scanned.
	if _, err := NewLoader().Load(ctx, PluginsConfig{
		Dirs: []string{filepath.Join(t.TempDir(), "nope")},
	}); err != nil {
		t.Fatalf("Load with nothing enabled must not touch dirs: %v", err)
	}
}
