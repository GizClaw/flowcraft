package deploy_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
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
	return resource.Spec{Kind: resource.Kind(f.kind), Impl: f.impl}
}

func (f expandRecorderFactory) New(_ context.Context, in resource.Input) (any, error) {
	var settings struct {
		Root string `json:"root"`
		Name string `json:"name"`
	}
	if err := resource.DecodeSettings(context.Background(), &settings, in.Settings); err != nil {
		return nil, err
	}
	*f.records = append(*f.records, settings.Root+"|"+settings.Name)
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
		Root string `json:"root"`
	}
	if err := resource.DecodeSettings(context.Background(), &settings, in.Settings); err != nil {
		return nil, err
	}
	*f.records = append(*f.records, settings.Root)
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
		Root string `json:"root"`
	}
	if err := resource.DecodeSettings(context.Background(), &settings, in.Settings); err != nil {
		return nil, err
	}
	*f.records = append(*f.records, settings.Root)
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
		Fn: func(_ context.Context, ref string) (string, error) {
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
