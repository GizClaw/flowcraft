package catalog

import (
	"github.com/GizClaw/flowcraft/sdk/history"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	// Side-effect imports: each sdkx LLM provider package
	// registers itself with llm.DefaultRegistry in its init().
	// Importing them here means the daemon binary always carries
	// every supported provider regardless of which LLMProfile docs
	// the user wires; users avoid them at runtime by simply not
	// referencing the provider — the registration is cheap
	// (function pointer in a map).
	_ "github.com/GizClaw/flowcraft/sdkx/llm/anthropic"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/bytedance"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/deepseek"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/mock"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/openai"
)

// Builtin returns a Catalog pre-populated with every in-tree
// factory v0.1.0 ships. Forks of vesseld can call New() instead
// and selectively re-register, but the common path is to start
// from Builtin() and append custom factories.
func Builtin() *Catalog {
	c := New()
	registerBuiltinEngines(c)
	registerBuiltinProbes(c)
	registerBuiltinToolPacks(c)
	registerBuiltinLLMProviders(c)
	registerBuiltinHistories(c)
	return c
}

// registerBuiltinEngines wires the v0.1.0 engine factories. We
// keep a single "graph-llm" entry that covers the standard
// "single LLM node + optional tool node" shape; richer factories
// (graph-recall, graph-knowledge) can layer in v0.2.0+.
func registerBuiltinEngines(c *Catalog) {
	c.RegisterEngine("graph-llm", graphLLMEngineFactory)
}

// registerBuiltinProbes wires the v0.1.0 probe factories. The only
// shipped probe is "llm-reachable", which pings an LLMProfile.
func registerBuiltinProbes(c *Catalog) {
	c.RegisterProbe("llm-reachable", func(ref string, cfg map[string]any, deps Deps) (spec.Probe, error) {
		profile, _ := cfg["llmProfile"].(string)
		if profile == "" {
			return nil, formatRefError("vesseld probe", ref, "config.llmProfile is required")
		}
		client := deps.LLMClients[profile]
		if client == nil {
			return nil, formatRefError("vesseld probe", ref, "LLMProfile %q not found in daemon LLMClients map", profile)
		}
		label, _ := cfg["label"].(string)
		if label == "" {
			label = "llm-reachable/" + profile
		}
		// vessel.LLMReachableProbe wants an llm.LLMResolver; wrap
		// the per-profile client in a single-entry resolver shim
		// so the existing probe implementation works without
		// modification.
		return &vessel.LLMReachableProbe{
			Resolver: &fixedClientResolver{client: client},
			Model:    profile,
			Label:    label,
		}, nil
	})

	// token-budget: trips when the vessel's hourly token total
	// reaches Threshold (default 0.8). The probe needs the running
	// captain's *tokenBudget; vessel.Captain.New wires it by
	// type-asserting the slice and calling the package-private
	// setBudget. The factory therefore returns a "blank" probe —
	// it becomes useful only once a Captain anchors it.
	c.RegisterProbe("token-budget", func(_ string, cfg map[string]any, _ Deps) (spec.Probe, error) {
		threshold := 0.0
		switch v := cfg["threshold"].(type) {
		case float64:
			threshold = v
		case float32:
			threshold = float64(v)
		}
		label, _ := cfg["label"].(string)
		return &vessel.TokenBudgetProbe{Threshold: threshold, Label: label}, nil
	})

	// tool-reachable: confirms a named Tool is still in the
	// shared registry the daemon hands to engines. Useful as a
	// canary for "did somebody unregister my custom tool?" and
	// for vessels that hard-depend on the kanban_submit /
	// task_context auto-tools.
	c.RegisterProbe("tool-reachable", func(ref string, cfg map[string]any, deps Deps) (spec.Probe, error) {
		toolName, _ := cfg["toolName"].(string)
		if toolName == "" {
			return nil, formatRefError("vesseld probe", ref, "config.toolName is required")
		}
		if deps.ToolRegistry == nil {
			return nil, formatRefError("vesseld probe", ref, "tool registry not initialised")
		}
		label, _ := cfg["label"].(string)
		return &vessel.ToolReachableProbe{
			Registry: deps.ToolRegistry,
			ToolName: toolName,
			Label:    label,
		}, nil
	})
}

// registerBuiltinToolPacks ships zero v0.1.0 tool packs. The
// kanban_submit / task_context tools are auto-injected by vessel
// (not catalog-driven) and recall / knowledge tool packs land in
// v0.2.0+. The category exists because user-side configurations
// can already reference future packs without schema churn.
func registerBuiltinToolPacks(_ *Catalog) {
	// intentionally empty in v0.1.0
}

// registerBuiltinLLMProviders maps each sdkx provider name to a
// normalisation factory. The factory's job is to copy the
// LLMProfile.spec.config map into a llm.ProviderConfig.Config
// shape the underlying llm.RegisterProvider hook expects, and to
// inject the resolved api key under the conventional "api_key"
// key the sdkx providers all read.
//
// The five providers we wire mirror sdkx/llm/*; adding ollama /
// qwen / azure is a single line each once those providers stabilise.
func registerBuiltinLLMProviders(c *Catalog) {
	for _, name := range []string{"openai", "anthropic", "deepseek", "minimax", "bytedance"} {
		c.RegisterLLMProvider(name, makeProviderConfigFactory(name))
	}
}

// makeProviderConfigFactory returns a closure that turns one
// LLMProfile into a llm.ProviderConfig. Shared body across all
// in-tree providers — they all read api_key + base_url + (optional)
// timeout from the same shape.
func makeProviderConfigFactory(provider string) LLMProviderFactoryFn {
	return func(profileName string, profileCfg map[string]any, apiKey string) (llm.ProviderConfig, error) {
		// Start with the user-supplied config so unknown keys
		// reach the provider unchanged (lets the provider evolve
		// its config independently of catalog releases).
		cfg := make(map[string]any, len(profileCfg)+1)
		for k, v := range profileCfg {
			cfg[k] = v
		}
		// Override api_key with the resolved value: the user
		// must supply it via valueFrom; inline values are
		// rejected at apispec validation time, so we never see
		// them here.
		cfg["api_key"] = apiKey
		// Convenience aliases: sdkx providers historically read
		// snake_case, but a user's first instinct in YAML may be
		// camelCase. Normalise the two most common keys.
		if v, ok := profileCfg["baseURL"]; ok && cfg["base_url"] == nil {
			cfg["base_url"] = v
		}
		if v, ok := profileCfg["defaultModel"]; ok && cfg["default_model"] == nil {
			cfg["default_model"] = v
		}
		return llm.ProviderConfig{
			Provider: provider,
			Profile:  profileName,
			Config:   cfg,
		}, nil
	}
}

// registerBuiltinHistories wires the in-tree history.History
// implementations available in v0.1.0. Only "buffer" ships:
// "compacted" requires a SummaryDAG + workspace pairing the
// daemon does not yet model declaratively, so it is intentionally
// deferred to v0.2.0+ rather than half-wired here.
//
// The buffer factory wraps an in-memory store sized by the
// caller's maxMessages config; default is 100. A single instance
// is shared across every Vessel that references the HistoryStore
// name, so cross-vessel conversations live on one transcript.
func registerBuiltinHistories(c *Catalog) {
	c.RegisterHistory("buffer", func(ref string, cfg map[string]any, _ Deps) (history.History, error) {
		max := intFromAny(cfg["maxMessages"], 100)
		store := history.NewInMemoryStore(history.WithMaxConversations(max))
		return history.NewBuffer(store), nil
	})
}

// intFromAny coerces any numeric YAML value (yaml.v3 hands us
// either int or int64 depending on size) into a plain int with a
// caller-supplied default for missing / wrong-type values.
func intFromAny(v any, def int) int {
	switch x := v.(type) {
	case int:
		return x
	case int32:
		return int(x)
	case int64:
		return int(x)
	case float64:
		return int(x)
	default:
		return def
	}
}
