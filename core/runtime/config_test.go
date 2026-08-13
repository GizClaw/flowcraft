package runtime

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
)

func parseRuntimeDocument(t *testing.T, runtimeYAML string) deploy.Document {
	t.Helper()
	doc, err := deploy.Parse([]byte(
		"version: v1\nagents: {}\nruntime:\n" + runtimeYAML))
	if err != nil {
		t.Fatalf("deploy.Parse: %v", err)
	}
	return doc
}

func TestDecodeConfigStrictAndValidated(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		doc := parseRuntimeDocument(t, `  event_bus: events
  checkpoint_store: cps
  sessions:
    idle_timeout: 30s
    sink_buffer: 17
    speculative_buffer_events: 23
    speculative_buffer_bytes: 4096
    delivery_concurrency: 3
    max_sessions: 7
    resume: true
`)
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		if cfg.EventBus != "events" || cfg.CheckpointStore != "cps" ||
			cfg.Sessions.IdleTimeout != 30*time.Second ||
			cfg.Sessions.SinkBuffer != 17 ||
			cfg.Sessions.SpeculativeBufferEvents != 23 ||
			cfg.Sessions.SpeculativeBufferBytes != 4096 ||
			cfg.Sessions.DeliveryConcurrency != 3 ||
			cfg.Sessions.MaxSessions != 7 ||
			!cfg.Sessions.Resume {
			t.Fatalf("unexpected config: %#v", cfg)
		}
	})

	t.Run("defaults", func(t *testing.T) {
		doc := parseRuntimeDocument(t, "  event_bus: events\n")
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		if cfg.Sessions.IdleTimeout != defaultIdleTimeout ||
			cfg.Sessions.SinkBuffer != defaultSinkBuffer ||
			cfg.Sessions.SpeculativeBufferEvents != defaultSpeculativeBufferEvents ||
			cfg.Sessions.SpeculativeBufferBytes != defaultSpeculativeBufferBytes ||
			cfg.Sessions.DeliveryConcurrency != defaultDeliveryConcurrency ||
			cfg.Sessions.MaxSessions != defaultMaxSessions ||
			cfg.Sessions.Resume {
			t.Fatalf("defaults not applied: %#v", cfg.Sessions)
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
		"absent":                   "",
		"empty":                    "  {}\n",
		"unknown runtime field":    "  event_bus: events\n  surprise: true\n",
		"bad duration":             "  event_bus: events\n  sessions: {idle_timeout: soon}\n",
		"bad sink buffer":          "  event_bus: events\n  sessions: {sink_buffer: -1}\n",
		"bad speculative events":   "  event_bus: events\n  sessions: {speculative_buffer_events: 0}\n",
		"bad speculative bytes":    "  event_bus: events\n  sessions: {speculative_buffer_bytes: -1}\n",
		"huge sink buffer":         "  event_bus: events\n  sessions: {sink_buffer: 2097152}\n",
		"huge speculative events":  "  event_bus: events\n  sessions: {speculative_buffer_events: 2097152}\n",
		"huge speculative bytes":   "  event_bus: events\n  sessions: {speculative_buffer_bytes: 536870912}\n",
		"bad delivery concurrency": "  event_bus: events\n  sessions: {delivery_concurrency: 0}\n",
		"bad max sessions":         "  event_bus: events\n  sessions: {max_sessions: 0}\n",
		"resume without store":     "  event_bus: events\n  sessions: {resume: true}\n",
	} {
		t.Run(name, func(t *testing.T) {
			var doc deploy.Document
			if name == "absent" {
				doc = deploy.Document{Version: "v1"}
			} else {
				doc = parseRuntimeDocument(t, runtimeYAML)
			}
			if _, err := DecodeConfig(doc); err == nil {
				t.Fatal("DecodeConfig unexpectedly succeeded")
			}
		})
	}
}

func TestDecodeConfig_DynamicCatalog(t *testing.T) {
	t.Run("valid", func(t *testing.T) {
		doc := parseRuntimeDocument(t, `  event_bus: events
  dynamic_catalog:
    tools: {default: shared_tools, researcher: research_tools}
`)
		cfg, err := DecodeConfig(doc)
		if err != nil {
			t.Fatalf("DecodeConfig: %v", err)
		}
		dc := cfg.DynamicCatalog
		if dc == nil {
			t.Fatal("DynamicCatalog is nil")
		}
		if dc.Tools["default"] != "shared_tools" ||
			dc.Tools["researcher"] != "research_tools" {
			t.Fatalf("Tools = %#v", dc.Tools)
		}
	})

	for name, runtimeYAML := range map[string]string{
		"missing tools": `  event_bus: events
  dynamic_catalog: {}
`,
		"empty agent key": `  event_bus: events
  dynamic_catalog:
    tools: {'': research_tools}
`,
		"empty resource": `  event_bus: events
  dynamic_catalog:
    tools: {researcher: ''}
`,
	} {
		t.Run(name, func(t *testing.T) {
			doc := parseRuntimeDocument(t, runtimeYAML)
			if _, err := DecodeConfig(doc); !errdefs.IsValidation(err) {
				t.Fatalf("DecodeConfig error = %v, want validation", err)
			}
		})
	}
}

func TestDecodeConfigRejectsUnknownSessionField(t *testing.T) {
	doc := parseRuntimeDocument(t, `  event_bus: events
  sessions:
    idle_timeout: 30s
    mystery: true
`)
	_, err := DecodeConfig(doc)
	if err == nil || !strings.Contains(err.Error(), "mystery") {
		t.Fatalf("DecodeConfig error = %v, want unknown field mention", err)
	}
}
