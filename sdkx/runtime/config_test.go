package runtime

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/event"
	"github.com/GizClaw/flowcraft/sdkx/deploy"
	"github.com/GizClaw/flowcraft/sdkx/runtime/session"
	"github.com/GizClaw/flowcraft/sdkx/tool/dynamic"
)

func parseRuntimeDocument(t *testing.T, runtimeYAML string) deploy.Document {
	t.Helper()
	doc, err := deploy.Parse([]byte("version: v1\nagents: {}\nruntime:\n" + runtimeYAML))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}

func TestDecodeConfigStrictAndValidated(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		doc := parseRuntimeDocument(t, "  event_bus: events\n  checkpoint_store: cps\n  scheduler: ' shared-scheduler '\n  sessions:\n    idle_timeout: 30s\n    sink_buffer: 17\n    speculative_buffer_events: 23\n    speculative_buffer_bytes: 4096\n    resume: true\n  integrations:\n    - name: one\n      kind: test.kind\n      deps: {store: data}\n      settings: {answer: 42}\n")
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		if cfg.EventBus != "events" || cfg.CheckpointStore != "cps" ||
			cfg.Scheduler != "shared-scheduler" ||
			cfg.Sessions.IdleTimeout != 30*time.Second || cfg.Sessions.SinkBuffer != 17 ||
			cfg.Sessions.SpeculativeBufferEvents != 23 ||
			cfg.Sessions.SpeculativeBufferBytes != 4096 || !cfg.Sessions.Resume {
			t.Fatalf("unexpected config: %#v", cfg)
		}
		if len(cfg.Integrations) != 1 || cfg.Integrations[0].Settings == nil {
			t.Fatalf("unexpected integrations: %#v", cfg.Integrations)
		}
	})

	t.Run("blank scheduler is disabled", func(t *testing.T) {
		doc := parseRuntimeDocument(t, "  event_bus: events\n  scheduler: '   '\n")
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		if cfg.Scheduler != "" {
			t.Fatalf("Scheduler = %q, want empty", cfg.Scheduler)
		}
	})

	t.Run("blank checkpoint store is disabled", func(t *testing.T) {
		doc := parseRuntimeDocument(t, "  event_bus: events\n  checkpoint_store: '   '\n")
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		if cfg.CheckpointStore != "" {
			t.Fatalf("CheckpointStore = %q, want empty", cfg.CheckpointStore)
		}
	})

	for name, runtimeYAML := range map[string]string{
		"absent":                 "",
		"empty":                  "  {}\n",
		"unknown runtime field":  "  event_bus: events\n  surprise: true\n",
		"duplicate name":         "  event_bus: events\n  integrations:\n    - {name: same, kind: a}\n    - {name: same, kind: a}\n",
		"empty kind":             "  event_bus: events\n  integrations:\n    - {name: one, kind: ''}\n",
		"bad duration":           "  event_bus: events\n  sessions: {idle_timeout: soon}\n",
		"bad sink buffer":        "  event_bus: events\n  sessions: {sink_buffer: -1}\n",
		"bad speculative events": "  event_bus: events\n  sessions: {speculative_buffer_events: 0}\n",
		"bad speculative bytes":  "  event_bus: events\n  sessions: {speculative_buffer_bytes: -1}\n",
		"resume without store":   "  event_bus: events\n  sessions: {resume: true}\n",
	} {
		t.Run(name, func(t *testing.T) {
			var doc deploy.Document
			if name == "absent" {
				doc = deploy.Document{Version: deploy.VersionV1, Agents: map[string]deploy.AgentEntry{}}
			} else {
				doc = parseRuntimeDocument(t, runtimeYAML)
			}
			if _, err := DecodeConfig(doc); err == nil {
				t.Fatal("DecodeConfig unexpectedly succeeded")
			}
		})
	}
}

type testFactory struct {
	spec IntegrationSpec
}

func (f *testFactory) Spec() IntegrationSpec { return f.spec }
func (f *testFactory) Prepare(_ context.Context, _ PrepareInput) (PreparedIntegration, error) {
	return &testPrepared{}, nil
}

type testPrepared struct{}

func (*testPrepared) Bind(context.Context, BindInput) error { return nil }
func (*testPrepared) DecorateHost(factory session.HostFactory) (session.HostFactory, error) {
	return factory, nil
}
func (*testPrepared) Start(context.Context) error { return nil }
func (*testPrepared) Close() error                { return nil }

func TestRegisterIntegrationRejectsInvalidFactoriesAndSpecs(t *testing.T) {
	builder := NewBuilder(nil)
	var nilFactory *testFactory
	if err := builder.RegisterIntegration(nilFactory); err == nil {
		t.Fatal("typed-nil factory accepted")
	}
	if err := builder.RegisterIntegration(&testFactory{spec: IntegrationSpec{
		Kind: "bad",
		Deps: []DependencySpec{
			{Name: "same", Kind: "x", Type: reflect.TypeFor[event.Bus]()},
			{Name: "same", Kind: "x", Type: reflect.TypeFor[event.Bus]()},
		},
	}}); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("duplicate dep error = %v", err)
	}
	good := &testFactory{spec: IntegrationSpec{Kind: "good"}}
	if err := builder.RegisterIntegration(good); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := builder.RegisterIntegration(good); err == nil {
		t.Fatal("duplicate kind accepted")
	}
}

func TestDecodeConfig_DynamicCatalog(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		doc := parseRuntimeDocument(t, `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {default: shared_tools, researcher: research_tools}
      default_exposure: direct
      exposures: {tool_search: always, exec: hidden}
      selected_retention: 3
      recent_window: 7
      budget: {max_definitions: 20, max_bytes: 4096}
`)
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		dc := cfg.Sessions.DynamicCatalog
		if dc == nil {
			t.Fatal("DynamicCatalog is nil")
		}
		if dc.Tools["default"] != "shared_tools" ||
			dc.Tools["researcher"] != "research_tools" {
			t.Fatalf("Tools = %#v", dc.Tools)
		}
		if dc.DefaultExposure != dynamic.ExposureDirect {
			t.Errorf("DefaultExposure = %q, want direct", dc.DefaultExposure)
		}
		if dc.Exposures["tool_search"] != dynamic.ExposureAlways ||
			dc.Exposures["exec"] != dynamic.ExposureHidden {
			t.Errorf("Exposures = %#v", dc.Exposures)
		}
		if dc.SelectedRetention != 3 || dc.RecentWindow != 7 {
			t.Errorf("retention/window = %d/%d, want 3/7",
				dc.SelectedRetention, dc.RecentWindow)
		}
		if dc.Budget.MaxDefinitions != 20 || dc.Budget.MaxBytes != 4096 {
			t.Errorf("budget = %+v, want 20/4096", dc.Budget)
		}
	})

	t.Run("omitted policy uses defaults", func(t *testing.T) {
		doc := parseRuntimeDocument(t, `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools}
`)
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		dc := cfg.Sessions.DynamicCatalog
		if dc.DefaultExposure != dynamic.ExposureDeferred {
			t.Errorf("DefaultExposure = %q, want deferred", dc.DefaultExposure)
		}
		if len(dc.Exposures) != 0 || dc.SelectedRetention != 0 ||
			dc.RecentWindow != 0 || dc.Budget.MaxDefinitions != 0 ||
			dc.Budget.MaxBytes != 0 {
			t.Errorf("unexpected non-default policy: %#v", dc)
		}
	})

	for name, runtimeYAML := range map[string]string{
		"missing tools": `  event_bus: events
  sessions:
    dynamic_catalog: {}
`,
		"empty agent key": `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {'': research_tools}
`,
		"empty resource": `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: ''}
`,
		"bad default exposure": `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools}
      default_exposure: sometimes
`,
		"bad exposure value": `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools}
      exposures: {exec: maybe}
`,
		"negative retention": `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools}
      selected_retention: -1
`,
		"negative budget": `  event_bus: events
  sessions:
    dynamic_catalog:
      tools: {researcher: research_tools}
      budget: {max_definitions: -2}
`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := parseRuntimeDocument(t, runtimeYAML)
			if _, err := DecodeConfig(doc); err == nil {
				t.Fatal("DecodeConfig unexpectedly succeeded")
			}
		})
	}
}
