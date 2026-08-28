package deploy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
	"github.com/GizClaw/flowcraft/core/secret"
)

// expandRecorder captures the settings its factory decoded, so tests can
// assert what the centralized expansion pass materialized.
type expandRecorder struct{}

type expandRecorderFactory struct {
	kind    string
	impl    string
	records *[]string
}

func (f expandRecorderFactory) Spec() resource.Spec {
	return resource.Spec{
		Kind: resource.Kind(f.kind),
		Impl: f.impl,
		Deps: []resource.DepSpec{{
			Name: "secrets", Type: "secret.Store", Required: false,
		}},
	}
}

func (f expandRecorderFactory) New(ctx context.Context, in resource.Input) (any, error) {
	var settings struct {
		Root resource.Secret `json:"root"`
		Name resource.Secret `json:"name"`
	}
	if err := resource.DecodeSettings(ctx, &settings, in.Settings); err != nil {
		return nil, err
	}
	root, err := settings.Root.Resolve(ctx, in.Secrets)
	if err != nil {
		return nil, err
	}
	name, err := settings.Name.Resolve(ctx, in.Secrets)
	if err != nil {
		return nil, err
	}
	*f.records = append(*f.records, root+"|"+name)
	return &expandRecorder{}, nil
}

type expandEngine struct{}

func (expandEngine) Execute(
	_ context.Context, _ agent.Run, _ agent.Host, board *agent.Board,
) (*agent.Board, error) {
	return board, nil
}

func (expandEngine) Close() error { return nil }

type expandEngineFactory struct {
	records *[]string
}

func (f expandEngineFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "engine.test", Impl: "expand-record"}
}

func (f expandEngineFactory) New(_ context.Context, in resource.Input) (any, error) {
	var settings struct {
		Root resource.Secret `json:"root"`
	}
	if err := resource.DecodeSettings(context.Background(), &settings, in.Settings); err != nil {
		return nil, err
	}
	root, err := settings.Root.Resolve(context.Background(), in.Secrets)
	if err != nil {
		return nil, err
	}
	*f.records = append(*f.records, root)
	return expandEngine{}, nil
}

type expandHook struct {
	agent.BaseObserver
}

type expandHookFactory struct {
	records *[]string
}

func (f expandHookFactory) Spec() resource.Spec {
	return resource.Spec{Kind: "hook.observe", Impl: "expand-record"}
}

func (f expandHookFactory) New(_ context.Context, in resource.Input) (any, error) {
	var settings struct {
		Root resource.Secret `json:"root"`
	}
	if err := resource.DecodeSettings(context.Background(), &settings, in.Settings); err != nil {
		return nil, err
	}
	root, err := settings.Root.Resolve(context.Background(), in.Secrets)
	if err != nil {
		return nil, err
	}
	*f.records = append(*f.records, root)
	return &expandHook{}, nil
}

func TestBuilderExpandsResourceSettingsAllOpen(t *testing.T) {
	t.Setenv("EXPAND_TEST_ROOT", "/srv/flowcraft")
	var records []string
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	builder := deploy.NewBuilder(reg, deploy.WithLoader(
		resource.NewLoader(resource.WithBaseDir("/tmp/deploy"))))
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(
					`{"root": "${env:EXPAND_TEST_ROOT}", "name": "${base:sub}"}`),
			},
		},
	}
	if _, err := builder.Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "/srv/flowcraft|/tmp/deploy/sub" {
		t.Fatalf("decoded settings = %v, want env + base expanded", records)
	}
}

func TestBuilderPerResourceSettingsStayIsolated(t *testing.T) {
	t.Setenv("EXPAND_TEST_A", "value-a")
	t.Setenv("EXPAND_TEST_B", "value-b")
	var records []string
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{"root": "${env:EXPAND_TEST_A}"}`),
			},
			"b": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{"root": "${env:EXPAND_TEST_B}"}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 2 || records[0] != "value-a|" || records[1] != "value-b|" {
		t.Fatalf("decoded settings = %v, want per-resource values", records)
	}
}

func TestBuilderExpandsEngineAndHookSettings(t *testing.T) {
	t.Setenv("EXPAND_TEST_ENGINE", "engine-ok")
	t.Setenv("EXPAND_TEST_HOOK", "hook-ok")
	var records []string
	reg := resource.NewRegistry()
	reg.MustRegister(expandEngineFactory{records: &records})
	reg.MustRegister(expandHookFactory{records: &records})
	doc := deploy.Document{
		Version: "v1",
		Agents: map[string]agent.Definition{
			"a": {
				Card: agent.AgentCard{Name: "A"},
				Engine: agent.EngineRef{
					Kind:     "engine.test",
					Impl:     "expand-record",
					Settings: json.RawMessage(`{"root": "${env:EXPAND_TEST_ENGINE}"}`),
				},
				Observe: []agent.Hook{{
					Type:     "expand-record",
					Settings: json.RawMessage(`{"root": "${env:EXPAND_TEST_HOOK}"}`),
				}},
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	joined := strings.Join(records, ",")
	if !strings.Contains(joined, "engine-ok") || !strings.Contains(joined, "hook-ok") {
		t.Fatalf("decoded settings = %v, want engine + hook expanded", records)
	}
}

func TestBuilderEscapeAndCustomScheme(t *testing.T) {
	var records []string
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	resolver := resource.NewResolver(resource.SchemeFunc{
		SchemeName: "cfg",
		Fn: func(_ context.Context, ref string) (any, error) {
			if ref == "x" {
				return "cfg-value", nil
			}
			return "", errdefs.Validationf("cfg: unknown ref %q", ref)
		},
	})
	builder := deploy.NewBuilder(reg, deploy.WithResolver(resolver))
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(
					`{"root": "${cfg:x}", "name": "\\${env:NOPE}"}`),
			},
		},
	}
	if _, err := builder.Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "cfg-value|${env:NOPE}" {
		t.Fatalf("decoded settings = %v, want custom scheme + escaped literal", records)
	}
}

func TestBuilderCustomResolverKeepsBuiltinSchemes(t *testing.T) {
	t.Setenv("EXPAND_TEST_ROOT", "/srv/flowcraft")
	var records []string
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	resolver := resource.NewResolver(resource.SchemeFunc{
		SchemeName: "cfg",
		Fn: func(_ context.Context, ref string) (any, error) {
			if ref == "x" {
				return "cfg-value", nil
			}
			return "", errdefs.Validationf("cfg: unknown ref %q", ref)
		},
	})
	builder := deploy.NewBuilder(reg, deploy.WithResolver(resolver))
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(
					`{"root": "${env:EXPAND_TEST_ROOT}", "name": "${cfg:x}"}`),
			},
		},
	}
	if _, err := builder.Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "/srv/flowcraft|cfg-value" {
		t.Fatalf("decoded settings = %v, want builtin env + custom cfg merged", records)
	}
}

func TestBuilderDefersBoardReferences(t *testing.T) {
	var records []string
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{
					"root": "${board.user.name}",
					"name": "\\${board.x}"
				}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "${board.user.name}|\\${board.x}" {
		t.Fatalf("decoded settings = %v, want board refs deferred", records)
	}
}

func TestBuilderExpansionIsStrict(t *testing.T) {
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &[]string{},
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{"root": "${nope:value}"}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for unknown scheme", err)
	}
}

func TestBuilderMissingEnvFailsExpansion(t *testing.T) {
	key := "EXPAND_TEST_MISSING"
	if value, ok := os.LookupEnv(key); ok {
		t.Cleanup(func() { _ = os.Setenv(key, value) })
		if err := os.Unsetenv(key); err != nil {
			t.Fatalf("unset %s: %v", key, err)
		}
	}
	reg := resource.NewRegistry()
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &[]string{},
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(fmt.Sprintf(
					`{"root": "${env:%s}"}`, key)),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for missing env", err)
	}
}

// customSecretStore is an externally registered secret.Store impl:
// downstream backends plug in without touching core.
type customSecretStore struct{}

func (customSecretStore) Lookup(_ context.Context, name string) (string, bool, error) {
	if name == "external" {
		return "external-value", true, nil
	}
	return "", false, nil
}

func (customSecretStore) DefaultSecretStore() bool { return false }

type customSecretFactory struct{}

func (customSecretFactory) Spec() resource.Spec {
	return resource.Spec{Kind: secret.ResourceKind, Impl: "custom"}
}

func (customSecretFactory) New(context.Context, resource.Input) (any, error) {
	return customSecretStore{}, nil
}

func TestBuilderSecretStoresFeedSecretScheme(t *testing.T) {
	t.Setenv("SECRET_TEST_TOKEN", "tok-123")
	var records []string
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	reg.MustRegister(customSecretFactory{})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.env": {
				Kind: secret.ResourceKind, Impl: "env",
				Settings: json.RawMessage(`{"default": true}`),
			},
			"custom": {
				Kind: secret.ResourceKind, Impl: "custom",
			},
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{
					"root": "${secret:SECRET_TEST_TOKEN}",
					"name": "${secret:custom.external}"
				}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "tok-123|external-value" {
		t.Fatalf("decoded settings = %v, want default + named store secrets", records)
	}
}

func TestBuilderSecretStoreAsDependency(t *testing.T) {
	t.Setenv("SECRET_TEST_TOKEN", "tok-123")
	var records []string
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.env": {
				Kind: secret.ResourceKind, Impl: "env",
				Settings: json.RawMessage(`{"id": "env"}`),
			},
			"a": {
				Kind: "expand.test", Impl: "record",
				Deps: resource.Deps{"secrets": "secret.env"},
				Settings: json.RawMessage(
					`{"root": "${secret:env.SECRET_TEST_TOKEN}"}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "tok-123|" {
		t.Fatalf("decoded settings = %v, want named store secret", records)
	}
}

func TestBuilderRejectsMultipleDefaultSecretStores(t *testing.T) {
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.one": {
				Kind: secret.ResourceKind, Impl: "env",
				Settings: json.RawMessage(`{"default": true}`),
			},
			"secret.two": {
				Kind: secret.ResourceKind, Impl: "env",
				Settings: json.RawMessage(`{"default": true}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for multiple defaults", err)
	}
}

func TestBuilderSecretReferenceWithoutDefaultStoreFails(t *testing.T) {
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &[]string{},
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.env": {
				Kind: secret.ResourceKind, Impl: "env",
			},
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{"root": "${secret:SECRET_TEST_TOKEN}"}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); !errdefs.IsValidation(err) {
		t.Fatalf("Build error = %v, want validation for NAME-only ref without default", err)
	}
}

func TestBuilderFileSecretStore(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "token"), []byte("tok-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var records []string
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	reg.MustRegister(expandRecorderFactory{
		kind: "expand.test", impl: "record", records: &records,
	})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.files": {
				Kind: secret.ResourceKind, Impl: "file",
				Settings: json.RawMessage(
					`{"id": "files", "base": "` + dir + `"}`),
			},
			"a": {
				Kind: "expand.test", Impl: "record",
				Settings: json.RawMessage(`{"root": "${secret:files.token}"}`),
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Build(context.Background(), doc); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(records) != 1 || records[0] != "tok-file|" {
		t.Fatalf("decoded settings = %v, want file-backed secret", records)
	}
}

func TestBuilderSecretSchemeInEngineAndHookSettings(t *testing.T) {
	t.Setenv("SECRET_TEST_ENGINE", "engine-secret")
	t.Setenv("SECRET_TEST_HOOK", "hook-secret")
	var records []string
	reg := resource.NewRegistry()
	if err := secret.Register(reg); err != nil {
		t.Fatalf("secret.Register: %v", err)
	}
	reg.MustRegister(expandEngineFactory{records: &records})
	reg.MustRegister(expandHookFactory{records: &records})
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"secret.env": {
				Kind: secret.ResourceKind, Impl: "env",
				Settings: json.RawMessage(`{"id": "env", "default": true}`),
			},
		},
		Agents: map[string]agent.Definition{
			"a": {
				Card: agent.AgentCard{Name: "A"},
				Engine: agent.EngineRef{
					Kind:     "engine.test",
					Impl:     "expand-record",
					Settings: json.RawMessage(`{"root": "${secret:SECRET_TEST_ENGINE}"}`),
				},
				Observe: []agent.Hook{{
					Type:     "expand-record",
					Settings: json.RawMessage(`{"root": "${secret:env.SECRET_TEST_HOOK}"}`),
				}},
			},
		},
	}
	if _, err := deploy.NewBuilder(reg).Deploy(context.Background(), doc); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	joined := strings.Join(records, ",")
	if !strings.Contains(joined, "engine-secret") || !strings.Contains(joined, "hook-secret") {
		t.Fatalf("decoded settings = %v, want engine + hook secrets resolved", records)
	}
}

type countingSecretStore struct {
	mu    sync.Mutex
	calls int
}

func (c *countingSecretStore) Lookup(context.Context, string) (string, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	return "counted", true, nil
}

func (c *countingSecretStore) DefaultSecretStore() bool { return false }

type countingSecretFactory struct{ store *countingSecretStore }

func (f countingSecretFactory) Spec() resource.Spec {
	return resource.Spec{Kind: secret.ResourceKind, Impl: "count"}
}

func (f countingSecretFactory) New(context.Context, resource.Input) (any, error) {
	return f.store, nil
}

func TestBuilderSecretCacheTTL(t *testing.T) {
	build := func(t *testing.T, opts ...deploy.BuilderOption) *countingSecretStore {
		t.Helper()
		store := &countingSecretStore{}
		reg := resource.NewRegistry()
		reg.MustRegister(countingSecretFactory{store: store})
		reg.MustRegister(expandRecorderFactory{
			kind: "expand.test", impl: "record", records: &[]string{},
		})
		doc := deploy.Document{
			Version: "v1",
			Resources: resource.Resources{
				"count": {Kind: secret.ResourceKind, Impl: "count"},
				"a": {
					Kind: "expand.test", Impl: "record",
					Settings: json.RawMessage(`{"root": "${secret:count.x}"}`),
				},
				"b": {
					Kind: "expand.test", Impl: "record",
					Settings: json.RawMessage(`{"root": "${secret:count.x}"}`),
				},
			},
		}
		if _, err := deploy.NewBuilder(reg, opts...).Build(context.Background(), doc); err != nil {
			t.Fatalf("Build: %v", err)
		}
		return store
	}

	t.Run("default cache hits", func(t *testing.T) {
		store := build(t)
		if store.calls != 1 {
			t.Fatalf("store calls = %d, want 1 with default cache", store.calls)
		}
	})
	t.Run("cache disabled", func(t *testing.T) {
		store := build(t, deploy.WithSecretCacheTTL(-1))
		if store.calls != 2 {
			t.Fatalf("store calls = %d, want 2 with cache disabled", store.calls)
		}
	})
}
