package catalog

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/engine/depname"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// stubLLM is a minimal llm.LLM for graph-llm engine tests. It
// records the GenerateOptions it received and returns a canned
// "no tool call, plain text" reply so the engine loop terminates
// after one iteration. Tests assert on stub.gotTools to verify
// which tool definitions reached the LLM layer.
type stubLLM struct {
	gotTools []llm.ToolDefinition
}

func (s *stubLLM) Generate(_ context.Context, _ []model.Message, opts ...llm.GenerateOption) (model.Message, llm.TokenUsage, error) {
	o := &llm.GenerateOptions{}
	for _, fn := range opts {
		fn(o)
	}
	s.gotTools = append([]llm.ToolDefinition(nil), o.Tools...)
	return model.NewTextMessage(model.RoleAssistant, "done"), llm.TokenUsage{}, nil
}

func (s *stubLLM) GenerateStream(ctx context.Context, msgs []model.Message, opts ...llm.GenerateOption) (llm.StreamMessage, error) {
	return nil, nil
}

func (s *stubLLM) ProviderName() string { return "stub" }
func (s *stubLLM) ModelID() string      { return "stub" }

// fakeTool is a minimal tool.Tool that records nothing — its only
// purpose is to populate the registry so we can prove the engine
// either filters or fails to filter the registry surface.
func fakeTool(name string) tool.Tool {
	return tool.FuncTool(
		model.ToolDefinition{Name: name, Description: name + " tool"},
		func(_ context.Context, _ string) (string, error) { return "ok", nil },
	)
}

// TestGraphLLM_AllowlistFiltersToolDefinitions pins gap #3.
//
// The agent's spec.Tools allow-list is the upper bound on the
// tool definitions the LLM sees. Tools not in the list MUST be
// stripped before Generate; absent allow-list means deny-all (the
// LLM gets a tools-empty Generate so it cannot emit any tool_call).
func TestGraphLLM_AllowlistFiltersToolDefinitions(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(fakeTool("calc"))
	reg.Register(fakeTool("shell"))   // dangerous tool the agent should NOT see
	reg.Register(fakeTool("network")) // also off-limits

	stub := &stubLLM{}
	deps := Deps{
		VesselID:     "v",
		AgentName:    "primary",
		AgentTools:   []string{"calc"}, // allow only calc
		ToolRegistry: reg,
		LLMClients:   map[string]llm.LLM{"openai-default": stub},
	}
	eng, err := graphLLMEngineFactory("graph-llm", map[string]any{
		"llmProfile":    "openai-default",
		"maxIterations": 1,
	}, deps)
	if err != nil {
		t.Fatalf("graphLLMEngineFactory: %v", err)
	}

	board := engine.NewBoard()
	board.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleUser, "hi"))
	if _, err := eng.Execute(context.Background(), engine.Run{ID: "r1"}, engine.NoopHost{}, board); err != nil {
		t.Fatalf("engine.Execute: %v", err)
	}

	got := map[string]bool{}
	for _, td := range stub.gotTools {
		got[td.Name] = true
	}
	if !got["calc"] {
		t.Errorf("LLM did not see allow-listed tool 'calc'")
	}
	if got["shell"] {
		t.Errorf("LLM saw 'shell' — allow-list not enforced")
	}
	if got["network"] {
		t.Errorf("LLM saw 'network' — allow-list not enforced")
	}
}

// TestGraphLLM_EmptyAllowlistDeniesAll asserts the strict default:
// an agent with no Tools declared sees zero tool definitions, even
// when the registry has tools available. Empty allow-list == deny.
func TestGraphLLM_EmptyAllowlistDeniesAll(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(fakeTool("calc"))

	stub := &stubLLM{}
	deps := Deps{
		VesselID:     "v",
		AgentName:    "primary",
		AgentTools:   nil, // no allow-list
		ToolRegistry: reg,
		LLMClients:   map[string]llm.LLM{"openai-default": stub},
	}
	eng, err := graphLLMEngineFactory("graph-llm", map[string]any{
		"llmProfile":    "openai-default",
		"maxIterations": 1,
	}, deps)
	if err != nil {
		t.Fatalf("graphLLMEngineFactory: %v", err)
	}
	board := engine.NewBoard()
	board.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleUser, "hi"))
	if _, err := eng.Execute(context.Background(), engine.Run{ID: "r1"}, engine.NoopHost{}, board); err != nil {
		t.Fatalf("engine.Execute: %v", err)
	}
	if len(stub.gotTools) != 0 {
		t.Fatalf("LLM saw %d tool defs with empty allow-list — want 0 (deny-all)", len(stub.gotTools))
	}
}

// TestGraphLLM_AllowlistEnforcedAtExecution is the runtime
// counterpart: even if the LLM somehow emits a tool_call for a
// name outside the allow-list (legacy prompt, jailbreak, schema
// drift), the engine MUST reject it with errdefs.PolicyDenied.
func TestGraphLLM_AllowlistEnforcedAtExecution(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(fakeTool("calc"))
	reg.Register(fakeTool("shell"))

	deps := Deps{
		VesselID:     "v",
		AgentName:    "primary",
		AgentTools:   []string{"calc"},
		ToolRegistry: reg,
	}
	allow := buildAllowSet(deps.AgentTools)

	// Simulated jailbreak: tool_call to "shell" with calc-only allow.
	out, err := executeToolCall(context.Background(), deps, allow, model.ToolCall{
		ID: "1", Name: "shell",
	})
	if err == nil {
		t.Fatalf("executeToolCall accepted a tool outside the allow-list (out=%q)", out)
	}
	if !errdefs.IsPolicyDenied(err) {
		t.Fatalf("err = %v, want errdefs.PolicyDenied", err)
	}

	// Allow-listed call must go through.
	out, err = executeToolCall(context.Background(), deps, allow, model.ToolCall{
		ID: "2", Name: "calc",
	})
	if err != nil {
		t.Fatalf("allow-listed calc rejected: %v", err)
	}
	if out != "ok" {
		t.Fatalf("calc returned %q, want \"ok\"", out)
	}
}

// TestGraphLLM_RunDepsAllowedNamesOverridesAgentTools is the
// regression for contract-audit Epic D / #11. Once the inline
// engine resolves the allow-list from engine.Run.Deps, the
// per-run policy gate (which agent.Run populates from
// agent.Agent.Tools) MUST take precedence over the legacy
// factory-time catalog.Deps.AgentTools closure.
//
// Setup: factory wires the LEGACY closure with [calc, shell]; the
// run carries Run.Deps[ToolAllowedNames] = [calc] only. Without
// the resolution change, the LLM would see both calc + shell
// because allowSet was computed at factory time. With the change,
// the LLM sees only calc.
func TestGraphLLM_RunDepsAllowedNamesOverridesAgentTools(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(fakeTool("calc"))
	reg.Register(fakeTool("shell"))

	stub := &stubLLM{}
	deps := Deps{
		VesselID:     "v",
		AgentName:    "primary",
		AgentTools:   []string{"calc", "shell"}, // legacy claim — superset
		ToolRegistry: reg,
		LLMClients:   map[string]llm.LLM{"openai-default": stub},
	}
	eng, err := graphLLMEngineFactory("graph-llm", map[string]any{
		"llmProfile":    "openai-default",
		"maxIterations": 1,
	}, deps)
	if err != nil {
		t.Fatalf("graphLLMEngineFactory: %v", err)
	}

	// Per-run override: the agent.Run-promoted allow-list permits
	// only calc. The inline engine MUST honour this over the
	// closure.
	runDeps := engine.NewDependencies()
	runDeps.Set(depname.ToolAllowedNames, []string{"calc"})

	board := engine.NewBoard()
	board.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleUser, "hi"))
	if _, err := eng.Execute(context.Background(),
		engine.Run{ID: "r1", Deps: runDeps},
		engine.NoopHost{}, board); err != nil {
		t.Fatalf("engine.Execute: %v", err)
	}

	got := map[string]bool{}
	for _, td := range stub.gotTools {
		got[td.Name] = true
	}
	if !got["calc"] {
		t.Errorf("LLM did not see calc (run.Deps allow-list dropped a permitted tool)")
	}
	if got["shell"] {
		t.Errorf("LLM saw shell — run.Deps allow-list did NOT override factory closure")
	}
}

// TestGraphLLM_RunDepsEmptyAllowedNamesDeniesAll asserts the
// fail-closed semantics at the new resolution path: an explicit
// empty []string under depname.ToolAllowedNames is a deliberate
// "no tools permitted" signal — even when the factory closure
// permits some. Without this rule a deployment that wants to
// strip tools per-run could not do so without rebuilding the
// factory.
func TestGraphLLM_RunDepsEmptyAllowedNamesDeniesAll(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(fakeTool("calc"))

	stub := &stubLLM{}
	deps := Deps{
		VesselID:     "v",
		AgentName:    "primary",
		AgentTools:   []string{"calc"}, // legacy: would permit calc
		ToolRegistry: reg,
		LLMClients:   map[string]llm.LLM{"openai-default": stub},
	}
	eng, err := graphLLMEngineFactory("graph-llm", map[string]any{
		"llmProfile":    "openai-default",
		"maxIterations": 1,
	}, deps)
	if err != nil {
		t.Fatalf("graphLLMEngineFactory: %v", err)
	}

	runDeps := engine.NewDependencies()
	runDeps.Set(depname.ToolAllowedNames, []string{}) // explicit deny-all

	board := engine.NewBoard()
	board.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleUser, "hi"))
	if _, err := eng.Execute(context.Background(),
		engine.Run{ID: "r1", Deps: runDeps},
		engine.NoopHost{}, board); err != nil {
		t.Fatalf("engine.Execute: %v", err)
	}
	if len(stub.gotTools) != 0 {
		t.Fatalf("LLM saw %d tools, want 0 (explicit empty allow-list MUST deny all)",
			len(stub.gotTools))
	}
}

// TestGraphLLM_AbsentRunDepsKeyFallsBackToAgentTools is the
// back-compat guard: callers (custom drivers, legacy tests) that
// build vessel without driving it through agent.Run won't
// populate engine.Run.Deps[ToolAllowedNames]. The inline engine
// MUST keep honouring the factory-time closure for them.
func TestGraphLLM_AbsentRunDepsKeyFallsBackToAgentTools(t *testing.T) {
	t.Parallel()

	reg := tool.NewRegistry()
	reg.Register(fakeTool("calc"))
	reg.Register(fakeTool("shell"))

	stub := &stubLLM{}
	deps := Deps{
		VesselID:     "v",
		AgentName:    "primary",
		AgentTools:   []string{"calc"}, // factory closure permits calc only
		ToolRegistry: reg,
		LLMClients:   map[string]llm.LLM{"openai-default": stub},
	}
	eng, err := graphLLMEngineFactory("graph-llm", map[string]any{
		"llmProfile":    "openai-default",
		"maxIterations": 1,
	}, deps)
	if err != nil {
		t.Fatalf("graphLLMEngineFactory: %v", err)
	}

	board := engine.NewBoard()
	board.AppendChannelMessage(engine.MainChannel, model.NewTextMessage(model.RoleUser, "hi"))
	// Note: engine.Run.Deps is nil — no run-deps key set.
	if _, err := eng.Execute(context.Background(),
		engine.Run{ID: "r1"}, engine.NoopHost{}, board); err != nil {
		t.Fatalf("engine.Execute: %v", err)
	}

	got := map[string]bool{}
	for _, td := range stub.gotTools {
		got[td.Name] = true
	}
	if !got["calc"] {
		t.Error("legacy fallback broken: calc missing from LLM-visible defs")
	}
	if got["shell"] {
		t.Error("legacy fallback broken: shell leaked through (closure permitted only calc)")
	}
}
