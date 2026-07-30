package agent_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
)

type fakeFactory struct {
	spec     agent.EngineSpec
	newCalls int
	lastCfg  agent.Config
	buildEng agent.Engine
	buildErr error
}

func (f *fakeFactory) Spec() agent.EngineSpec { return f.spec }

func (f *fakeFactory) New(_ context.Context, cfg agent.Config) (agent.Engine, error) {
	f.newCalls++
	f.lastCfg = cfg
	return f.buildEng, f.buildErr
}

func graphSpec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind: "graph",
		Capabilities: agent.Capabilities{
			SupportsResume:  true,
			EmitsCheckpoint: true,
		},
		Deps: []agent.DepSpec{
			{Name: "llm", Type: "inference.Profile", Required: true},
			{Name: "tools", Type: "tool.Catalog"},
		},
	}
}

func TestRegistry_RegisterAndLookup(t *testing.T) {
	r := agent.NewRegistry()
	f := &fakeFactory{spec: graphSpec()}
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}

	got, ok := r.Lookup("graph")
	if !ok || got != f {
		t.Fatalf("Lookup(graph) = (%v, %v), want the registered factory", got, ok)
	}
	if _, ok := r.Lookup("missing"); ok {
		t.Fatalf("Lookup(missing) must report ok=false")
	}
}

func TestRegistry_RejectsNilEmptyAndDuplicate(t *testing.T) {
	r := agent.NewRegistry()
	if err := r.Register(nil); err == nil {
		t.Fatal("Register(nil) must fail")
	}
	if err := r.Register(&fakeFactory{spec: agent.EngineSpec{}}); err == nil {
		t.Fatal("Register with empty kind must fail")
	}
	f := &fakeFactory{spec: graphSpec()}
	if err := r.Register(f); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := r.Register(&fakeFactory{spec: graphSpec()}); err == nil {
		t.Fatal("duplicate kind registration must fail — last-one-wins hides config bugs")
	}
}

func TestRegistry_SpecsSortedStable(t *testing.T) {
	r := agent.NewRegistry()
	for _, kind := range []string{"script", "graph", "inline"} {
		s := graphSpec()
		s.Kind = kind
		if err := r.Register(&fakeFactory{spec: s}); err != nil {
			t.Fatalf("Register(%s): %v", kind, err)
		}
	}
	specs := r.Specs()
	if len(specs) != 3 || specs[0].Kind != "graph" || specs[1].Kind != "inline" || specs[2].Kind != "script" {
		t.Fatalf("Specs() = %+v, want kinds sorted graph/inline/script", specs)
	}
}

func TestEngineSpec_RoundTripsThroughJSON(t *testing.T) {
	// The spec is the serialisable contract config loaders and
	// dashboards depend on; pin the wire keys so a field rename is
	// an explicit review event.
	data, err := json.Marshal(graphSpec())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var wire map[string]any
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	for _, key := range []string{"kind", "capabilities", "deps"} {
		if _, ok := wire[key]; !ok {
			t.Errorf("wire key %q missing in %s", key, data)
		}
	}
	caps, _ := wire["capabilities"].(map[string]any)
	if _, ok := caps["supports_resume"]; !ok {
		t.Errorf("capabilities.supports_resume missing in %s", data)
	}

	var back agent.EngineSpec
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatalf("round-trip Unmarshal: %v", err)
	}
	if back.Kind != "graph" || !back.Capabilities.SupportsResume || len(back.Deps) != 2 || back.Deps[0].Name != "llm" {
		t.Fatalf("round-trip = %+v", back)
	}
}

func TestConfig_DepLookup(t *testing.T) {
	cfg := agent.Config{Deps: map[string]any{"llm": "profile-x"}}
	if v, ok := cfg.Dep("llm"); !ok || v != "profile-x" {
		t.Fatalf("Dep(llm) = (%v, %v)", v, ok)
	}
	if _, ok := cfg.Dep("missing"); ok {
		t.Fatal("Dep(missing) must report ok=false")
	}
	var empty agent.Config
	if _, ok := empty.Dep("llm"); ok {
		t.Fatal("nil-map Config.Dep must be safe and report ok=false")
	}
}

func TestFactory_NewReceivesResolvedConfig(t *testing.T) {
	eng := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		return b, nil
	})
	f := &fakeFactory{spec: graphSpec(), buildEng: eng}
	cfg := agent.Config{Deps: map[string]any{"llm": "profile-x"}}

	got, err := f.New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, ok := got.(agent.EngineFunc); !ok {
		t.Fatalf("New returned %T, want the EngineFunc the factory built", got)
	}
	if f.newCalls != 1 {
		t.Fatalf("newCalls = %d, want 1", f.newCalls)
	}
	if v, _ := f.lastCfg.Dep("llm"); v != "profile-x" {
		t.Fatalf("factory saw cfg.Deps[llm] = %v, want profile-x", v)
	}
}
