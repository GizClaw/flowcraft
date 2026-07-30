package config_test

import (
	"context"
	"os"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/inference"
	config "github.com/GizClaw/flowcraft/sdkx/agent/config"
	yamlv3 "gopkg.in/yaml.v3"
)

// ---------- fakes ----------

type fakeEngineFactory struct {
	spec     agent.EngineSpec
	gotCfg   agent.Config
	newCalls int
}

func (f *fakeEngineFactory) Spec() agent.EngineSpec { return f.spec }

func (f *fakeEngineFactory) New(_ context.Context, cfg agent.Config) (agent.Engine, error) {
	f.newCalls++
	f.gotCfg = cfg
	return agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "ok"))
		return b, nil
	}), nil
}

type recordingHook struct{ agent.BaseHook }

type recordingBefore struct {
	window int
}

func (r recordingBefore) Before(_ context.Context, _ agent.Identity, req *agent.Request) (*agent.Board, error) {
	b := agent.NewBoard()
	b.AppendChannelMessage(agent.MainChannel, req.Message)
	b.SetVar("window", r.window)
	return b, nil
}

func graphSpec() agent.EngineSpec {
	return agent.EngineSpec{
		Kind:         "graph",
		Capabilities: agent.Capabilities{SupportsResume: true},
		Deps: []agent.DepSpec{
			{Name: "llm", Type: "inference.Profile", Required: true},
			{Name: "tools", Type: "tool.Catalog"},
		},
	}
}

func newTestBuilder(t *testing.T) (*config.Builder, *fakeEngineFactory, *fakeEngineFactory) {
	t.Helper()
	reg := agent.NewRegistry()
	graph := &fakeEngineFactory{spec: graphSpec()}
	inline := &fakeEngineFactory{spec: agent.EngineSpec{Kind: "inline"}}
	if err := reg.Register(graph); err != nil {
		t.Fatalf("register graph: %v", err)
	}
	if err := reg.Register(inline); err != nil {
		t.Fatalf("register inline: %v", err)
	}

	b := config.NewBuilder(reg)
	b.RegisterSource("inference.profile", func(_ context.Context, ref string) (any, error) {
		return "profile:" + ref, nil
	})
	b.RegisterSource("tool.catalog", func(_ context.Context, ref string) (any, error) {
		return "catalog:" + ref, nil
	})
	b.RegisterHook("transcript", func(_ context.Context, settings *yamlv3.Node) (agent.Hook, error) {
		type s struct {
			Store string `yaml:"store"`
		}
		if _, err := config.DecodeSettings[s](settings); err != nil {
			return nil, err
		}
		return &recordingHook{}, nil
	})
	b.RegisterBefore("history", func(_ context.Context, settings *yamlv3.Node) (agent.BeforeExecute, error) {
		type s struct {
			Window int `yaml:"window"`
		}
		dec, err := config.DecodeSettings[s](settings)
		if err != nil {
			return nil, err
		}
		return recordingBefore{window: dec.Window}, nil
	})
	return b, graph, inline
}

func loadDoc(t *testing.T) config.Document {
	t.Helper()
	data, err := os.ReadFile("testdata/agents.yaml")
	if err != nil {
		t.Fatalf("read testdata: %v", err)
	}
	doc, err := config.Parse(data)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return doc
}

// ---------- Parse ----------

func TestParse_HappyPath(t *testing.T) {
	doc := loadDoc(t)
	if doc.Version != config.VersionV1 {
		t.Fatalf("Version = %q", doc.Version)
	}
	if len(doc.Agents) != 2 {
		t.Fatalf("Agents = %v, want 2 entries", doc.Agents)
	}
	r := doc.Agents["researcher"]
	if r.Engine.Kind != "graph" || r.Engine.Settings["max_steps"] != 8 {
		t.Errorf("Engine = %+v", r.Engine)
	}
	if r.Deps["llm"].Source != "inference.profile" || r.Deps["llm"].Ref != "kimi-k2" {
		t.Errorf("Deps[llm] = %+v", r.Deps["llm"])
	}
	if r.Before == nil || r.Before.Type != "history" {
		t.Errorf("Before = %+v", r.Before)
	}
	if len(r.Hooks) != 1 || r.Hooks[0].Type != "transcript" {
		t.Errorf("Hooks = %+v", r.Hooks)
	}
	if len(r.After) != 1 || r.After[0].Type != "discard_on_interrupt" {
		t.Errorf("After = %+v", r.After)
	}
	if r.Policy.MaxRevise != 2 || len(r.Policy.ArtifactChannels) != 1 {
		t.Errorf("Policy = %+v", r.Policy)
	}
}

func TestParse_RejectsUnknownField(t *testing.T) {
	doc := []byte("version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    typo_field: 1\n")
	if _, err := config.Parse(doc); err == nil {
		t.Fatal("unknown field must fail strict parse")
	}
}

func TestParse_RejectsBadVersionAndTrailingDoc(t *testing.T) {
	if _, err := config.Parse([]byte("version: v2\nagents: {}\n")); err == nil {
		t.Fatal("unsupported version must fail")
	}
	if _, err := config.Parse([]byte("version: v1\nagents: {}\n---\nversion: v1\n")); err == nil {
		t.Fatal("trailing YAML document must fail")
	}
}

func TestParse_ValidateInvariants(t *testing.T) {
	cases := map[string]string{
		"missing engine.kind": "version: v1\nagents:\n  a:\n    engine: {settings: {}}\n",
		"dep missing source":  "version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    deps: {llm: {ref: x}}\n",
		"hook missing type":   "version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    hooks: [{settings: {}}]\n",
		"negative max_revise": "version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    policy: {max_revise: -1}\n",
	}
	for name, doc := range cases {
		if _, err := config.Parse([]byte(doc)); err == nil {
			t.Errorf("%s: must fail validation", name)
		}
	}
}

// ---------- Build ----------

func TestBuild_AssemblesRunnableInstance(t *testing.T) {
	b, graph, _ := newTestBuilder(t)
	doc := loadDoc(t)

	insts, err := b.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(insts) != 2 {
		t.Fatalf("instances = %v", insts)
	}

	r := insts["researcher"]
	if r.Agent.ID != "researcher" || r.Agent.Card.Name != "研究员" {
		t.Errorf("Agent = %+v", r.Agent)
	}
	if len(r.Agent.Tools) != 2 || r.Agent.Tools[0] != "search" {
		t.Errorf("Tools = %v", r.Agent.Tools)
	}

	// Factory saw resolved deps + opaque settings, pure data.
	if graph.newCalls != 1 {
		t.Fatalf("graph factory newCalls = %d", graph.newCalls)
	}
	if v, _ := graph.gotCfg.Dep("llm"); v != "profile:kimi-k2" {
		t.Errorf("cfg.Dep(llm) = %v", v)
	}
	if v, _ := graph.gotCfg.Dep("tools"); v != "catalog:default" {
		t.Errorf("cfg.Dep(tools) = %v", v)
	}
	if v, _ := graph.gotCfg.Setting("graph"); v != "graphs/research.yaml" {
		t.Errorf("cfg.Setting(graph) = %v", v)
	}

	// The instance is actually runnable end to end.
	res, err := r.Execute(context.Background(), agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("instance Execute: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Errorf("Status = %q", res.Status)
	}

	// Minimal agent builds without any deps/hooks.
	if _, err := insts["minimal"].Execute(context.Background(), agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
	}); err != nil {
		t.Fatalf("minimal Execute: %v", err)
	}
}

func TestBuild_UnknownEngineKind(t *testing.T) {
	b, _, _ := newTestBuilder(t)
	doc, err := config.Parse([]byte("version: v1\nagents:\n  a:\n    engine: {kind: ghost}\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := b.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered engine kind must fail Build")
	}
}

func TestBuild_DepValidation(t *testing.T) {
	b, _, _ := newTestBuilder(t)

	undeclared := "version: v1\nagents:\n  a:\n    engine: {kind: graph}\n    deps:\n      llm: {source: inference.profile, ref: x}\n      ghost: {source: inference.profile, ref: y}\n"
	doc, err := config.Parse([]byte(undeclared))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := b.Build(context.Background(), doc); err == nil {
		t.Fatal("dep not declared in EngineSpec must fail Build")
	}

	missingRequired := "version: v1\nagents:\n  a:\n    engine: {kind: graph}\n    deps:\n      tools: {source: tool.catalog}\n"
	doc, err = config.Parse([]byte(missingRequired))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := b.Build(context.Background(), doc); err == nil {
		t.Fatal("missing required dep must fail Build")
	}

	unknownSource := "version: v1\nagents:\n  a:\n    engine: {kind: graph}\n    deps:\n      llm: {source: nowhere, ref: x}\n"
	doc, err = config.Parse([]byte(unknownSource))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := b.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered source must fail Build")
	}
}

func TestBuild_UnknownHookType(t *testing.T) {
	b, _, _ := newTestBuilder(t)
	doc, err := config.Parse([]byte("version: v1\nagents:\n  a:\n    engine: {kind: inline}\n    hooks: [{type: ghost}]\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if _, err := b.Build(context.Background(), doc); err == nil {
		t.Fatal("unregistered hook type must fail Build")
	}
}

func TestBuild_BuiltinDiscardOnInterrupt(t *testing.T) {
	b, _, _ := newTestBuilder(t)
	doc := loadDoc(t)
	insts, err := b.Build(context.Background(), doc)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Drive the assembled instance to an interrupt: the built-in
	// disposition hook must mark the result uncommitted.
	intr := agent.EngineFunc(func(_ context.Context, _ agent.Run, _ agent.Host, b *agent.Board) (*agent.Board, error) {
		b.AppendChannelMessage(agent.MainChannel, inference.NewTextMessage(inference.RoleAssistant, "partial"))
		return b, agent.Interrupted(agent.Interrupt{Cause: agent.CauseUserInput})
	})
	inst := insts["researcher"]
	inst.Engine = intr

	res, err := inst.Execute(context.Background(), agent.Request{
		Message: inference.NewTextMessage(inference.RoleUser, "hi"),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.Status != agent.StatusInterrupted {
		t.Fatalf("Status = %q", res.Status)
	}
	if res.Committed {
		t.Error("builtin discard_on_interrupt must set Committed=false on user_input")
	}
}

func TestDecodeSettings_StrictAndNilSafe(t *testing.T) {
	type s struct {
		Window int `yaml:"window"`
	}
	if v, err := config.DecodeSettings[s](nil); err != nil || v.Window != 0 {
		t.Fatalf("nil node: (%v, %v)", v, err)
	}

	var node yamlv3.Node
	if err := yamlv3.Unmarshal([]byte("window: 3\n"), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, err := config.DecodeSettings[s](&node)
	if err != nil || v.Window != 3 {
		t.Fatalf("known field: (%v, %v)", v, err)
	}

	if err := yamlv3.Unmarshal([]byte("windo: 3\n"), &node); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, err := config.DecodeSettings[s](&node); err == nil {
		t.Fatal("typo key must fail strict decode")
	}
}
