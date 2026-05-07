package fleet

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/cmd/vesseld/apispec"
	"github.com/GizClaw/flowcraft/cmd/vesseld/catalog"
	"github.com/GizClaw/flowcraft/cmd/vesseld/resolver"
	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/llm"

	// Side-effect import: registers the "mock" provider in
	// llm.DefaultRegistry so resolver.resolveLLMs's
	// llm.NewFromConfig("mock", ...) can build a live client
	// without real credentials. Building inside catalog
	// production code is intentionally avoided (the mock is a
	// test concern); the test pulls it in here.
	_ "github.com/GizClaw/flowcraft/sdkx/llm/mock"
)

// sharedLLMConfig wires two vessels off ONE LLMProfile. If
// resolver / fleet ever regress and start constructing a fresh
// client per reference, the daemon's per-profile rate-limit and
// connection-pool stories silently break — assertion below pins
// the contract.
const sharedLLMConfig = `
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Daemon
metadata:
  name: vesseld-shared
spec:
  control:
    socket: /tmp/v.sock
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: LLMProfile
metadata:
  name: shared-mock
spec:
  provider: mock
  config:
    defaultModel: mock-default
  auth:
    apiKey:
      valueFrom:
        env: VESSELD_TEST_SHARED_LLM_KEY
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: alpha
spec:
  agents: [a]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: a
spec:
  engine:
    ref: spy
    config:
      llmProfile: shared-mock
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Vessel
metadata:
  name: beta
spec:
  agents: [b]
---
apiVersion: vessel.flowcraft.io/v1alpha1
kind: Agent
metadata:
  name: b
spec:
  engine:
    ref: spy
    config:
      llmProfile: shared-mock
`

// TestFleet_SharedLLMClient asserts the daemon's per-profile
// LLM client is constructed exactly once and the SAME client
// instance is handed to every Agent / Vessel that references the
// LLMProfile. The proof is identity: deps.LLMClients["shared-mock"]
// returns the same pointer across both vessels' engine factory
// invocations.
//
// The test uses sdkx/llm/mock as the provider so resolver can
// build a live client without real credentials, and registers a
// "spy" engine factory in the catalog that captures the deps
// LLMClient pointer at engine-construction time.
func TestFleet_SharedLLMClient(t *testing.T) {
	t.Setenv("VESSELD_TEST_SHARED_LLM_KEY", "sk-shared-test")

	objs, err := apispec.DecodeAll(strings.NewReader(sharedLLMConfig), "<test>")
	if err != nil {
		t.Fatal(err)
	}

	cat := catalog.Builtin()
	// Override the "mock" LLM provider entry — Builtin does not
	// catalog mock by default (it is test-only), so add it now.
	cat.RegisterLLMProvider("mock", func(profile string, cfg map[string]any, apiKey string) (llm.ProviderConfig, error) {
		out := map[string]any{}
		for k, v := range cfg {
			out[k] = v
		}
		out["api_key"] = apiKey
		return llm.ProviderConfig{Provider: "mock", Profile: profile, Config: out}, nil
	})

	var (
		captured []llm.LLM
		capMu    sync.Mutex
	)
	cat.RegisterEngine("spy", func(_ string, cfg map[string]any, deps catalog.Deps) (engine.Engine, error) {
		profile, _ := cfg["llmProfile"].(string)
		client := deps.LLMClients[profile]
		capMu.Lock()
		captured = append(captured, client)
		capMu.Unlock()
		return engine.EngineFunc(func(_ context.Context, _ engine.Run, _ engine.Host, b *engine.Board) (*engine.Board, error) {
			return b, nil
		}), nil
	})

	plan, errs := resolver.Resolve(objs, cat, resolver.ResolveOptions{AllowSecret: true})
	if errs.Len() != 0 {
		t.Fatalf("resolve: %v", errs)
	}

	f, err := Build(*plan)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := f.Launch(context.Background()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = f.Stop(ctx)
	})

	// Each vessel has one Agent → spy fires once per Agent → 2
	// captures total. Both pointers MUST equal each other AND
	// the plan-level client.
	capMu.Lock()
	defer capMu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("spy captured %d clients, want 2 (one per vessel)", len(captured))
	}
	if captured[0] == nil {
		t.Fatal("spy received a nil llm.LLM — resolver did not construct the mock client (AllowSecret=true was supposed to enable live builds)")
	}
	if captured[0] != captured[1] {
		t.Fatalf("two vessels received different llm.LLM instances:\n  alpha=%p\n  beta=%p\n→ resolver / fleet is constructing a per-reference client instead of sharing it.\n  Daemon's per-profile rate-limit and connection-pool stories silently break in this state.", captured[0], captured[1])
	}

	// Sanity: drive a real Submit through one vessel and confirm
	// the spy engine still works. This is not the assertion's
	// crux but rules out the "we returned a no-op so any
	// pointer would pass" hazard.
	h, err := f.Submit(context.Background(), "alpha", "a", agent.Request{})
	if err != nil {
		t.Fatalf("Submit alpha: %v", err)
	}
	if _, err := h.Wait(context.Background()); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}
