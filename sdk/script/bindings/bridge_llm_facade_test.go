package bindings

// White-box integration tests for NewLLMBridge — the script-facing facade
// (llm.run / llm.stream) was previously only validated indirectly through
// the round driver in bridge_llm_round_test.go. These tests exercise the
// full path: jsrt script → bridge → fake LLM → projected map back to the
// script. Keeping them in package bindings (not bindings_test) lets them
// reuse the fakeResolver / fakeLLM / fakeStream doubles already defined
// in bridge_llm_round_test.go.

import (
	"context"
	"errors"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/script"
	"github.com/GizClaw/flowcraft/sdk/script/jsrt"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// llmEnv assembles a script.Env that exposes only the "llm" global, backed
// by the supplied resolver / registry / read-history function. Mirrors the
// minimal wiring a host would do when only LLM access matters.
func llmEnv(t *testing.T, br BindingFunc) *script.Env {
	t.Helper()
	k, v := br(context.Background())
	return &script.Env{Bindings: map[string]any{k: v}}
}

func newScriptedLLM(text string) (*fakeResolver, *fakeLLM) {
	// model.StreamChunk carries text in Content (string), not a Part —
	// the bridge's roundStream.Next projects Content into a PartText
	// for script consumers. See bridge_llm_round.go.
	stream := &fakeStream{
		chunks: []model.StreamChunk{{Content: text}},
		final:  model.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: text}}},
		usage:  model.Usage{InputTokens: 1, OutputTokens: 2},
	}
	llmd := &fakeLLM{stream: stream}
	return &fakeResolver{llm: llmd}, llmd
}

// ---------------------------------------------------------------------------
// llm.run() — happy path + option / history wiring
// ---------------------------------------------------------------------------

func TestLLMBridge_Run_HappyPath(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res, llmd := newScriptedLLM("hi from model")

	bridge := NewLLMBridge(LLMBridgeOptions{
		Resolver: res,
		Defaults: LLMRunOptions{Model: "default-model"},
		Source:   "test",
		ReadMessages: func(_ context.Context) []model.Message {
			return []model.Message{{Role: model.RoleUser, Parts: []model.Part{{Type: model.PartText, Text: "hello"}}}}
		},
	})
	env := llmEnv(t, bridge)

	_, err := rt.Exec(context.Background(), "run", `
		var r = llm.run(null);
		if (r.content !== "hi from model") throw new Error("content: " + r.content);
		if (!r.usage || r.usage.input_tokens !== 1) throw new Error("usage missing");
		if (!r.messages || r.messages.length === 0) throw new Error("messages missing");
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}

	// History was forwarded to the LLM.
	if len(llmd.gotMsgs) != 1 || llmd.gotMsgs[0].Role != model.RoleUser {
		t.Errorf("LLM did not receive ReadMessages payload: %+v", llmd.gotMsgs)
	}
	if res.gotModel != "default-model" {
		t.Errorf("default model not used: %q", res.gotModel)
	}
}

func TestLLMBridge_Run_ScriptOverrideWinsOverDefaults(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res, _ := newScriptedLLM("ok")

	bridge := NewLLMBridge(LLMBridgeOptions{
		Resolver: res,
		Defaults: LLMRunOptions{Model: "default-model"},
		Source:   "test",
	})
	env := llmEnv(t, bridge)

	_, err := rt.Exec(context.Background(), "override", `
		llm.run({ model: "override-model" });
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if res.gotModel != "override-model" {
		t.Errorf("script override not honored: got %q", res.gotModel)
	}
}

func TestLLMBridge_Run_RejectsUnknownOption(t *testing.T) {
	// parseRunOptions uses DisallowUnknownFields — confirms typos in the
	// script reach Go as a real error rather than being silently dropped.
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res, _ := newScriptedLLM("ok")

	env := llmEnv(t, NewLLMBridge(LLMBridgeOptions{Resolver: res, Defaults: LLMRunOptions{Model: "m"}, Source: "test"}))
	_, err := rt.Exec(context.Background(), "typo", `
		try {
			llm.run({ modle: "typo" });
			throw new Error("expected llm.run to reject unknown field");
		} catch (e) {
			if (String(e).indexOf("modle") === -1) {
				throw new Error("error should name the bad field, got: " + e);
			}
		}
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
}

func TestLLMBridge_Run_ResolverErrorPropagates(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res := &fakeResolver{resolveErr: errors.New("provider unreachable")}

	env := llmEnv(t, NewLLMBridge(LLMBridgeOptions{Resolver: res, Defaults: LLMRunOptions{Model: "m"}, Source: "test"}))
	_, err := rt.Exec(context.Background(), "resolveerr", `
		try {
			llm.run(null);
			throw new Error("expected llm.run to throw on resolver error");
		} catch (e) {
			if (String(e).indexOf("provider unreachable") === -1) {
				throw new Error("error should surface resolver message, got: " + e);
			}
		}
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
}

// ---------------------------------------------------------------------------
// llm.stream() — iterator surface, finish, close
// ---------------------------------------------------------------------------

func TestLLMBridge_Stream_TextAccumulatesAcrossChunks(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	stream := &fakeStream{
		chunks: []model.StreamChunk{
			{Content: "Hel"},
			{Content: "lo "},
			{Content: "world"},
		},
		final: model.Message{Role: model.RoleAssistant, Parts: []model.Part{{Type: model.PartText, Text: "Hello world"}}},
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}

	env := llmEnv(t, NewLLMBridge(LLMBridgeOptions{Resolver: res, Defaults: LLMRunOptions{Model: "m"}, Source: "test"}))
	_, err := rt.Exec(context.Background(), "stream-text", `
		var s = llm.stream(null);
		var seen = "";
		while (s.next()) {
			seen += s.text();
		}
		var r = s.finish();
		if (seen !== "Hello world") throw new Error("accumulated: " + seen);
		if (r.content !== "Hello world") throw new Error("final content: " + r.content);
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
}

func TestLLMBridge_Stream_PartExposesToolCall(t *testing.T) {
	// model.StreamChunk carries either Content (text) or ToolCalls — the
	// bridge projects ToolCalls into a PartToolCall so script callers can
	// branch on `p.type === "tool_call"` without parsing raw JSON. This is
	// the only multimodal-style path achievable through StreamChunk today.
	rt := jsrt.New(jsrt.WithPoolSize(1))
	stream := &fakeStream{
		chunks: []model.StreamChunk{
			{ToolCalls: []model.ToolCall{{ID: "call-1", Name: "search", Arguments: `{"q":"x"}`}}},
		},
		final: model.Message{Role: model.RoleAssistant},
	}
	res := &fakeResolver{llm: &fakeLLM{stream: stream}}

	env := llmEnv(t, NewLLMBridge(LLMBridgeOptions{Resolver: res, Defaults: LLMRunOptions{Model: "m"}, Source: "test"}))
	_, err := rt.Exec(context.Background(), "stream-part", `
		var s = llm.stream(null);
		if (!s.next()) throw new Error("expected one chunk");
		var p = s.part();
		if (p.type !== "tool_call") throw new Error("part.type: " + p.type);
		if (!p.tool_call || p.tool_call.name !== "search") throw new Error("tool_call name lost");
		s.close();
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if !stream.closed {
		t.Error("stream.Close should have been called via s.close()")
	}
}

func TestLLMBridge_Stream_FinishAfterDrainReturnsResult(t *testing.T) {
	// Calling finish() after manual draining must still return the same
	// result (the round driver caches the result on first finish).
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res, _ := newScriptedLLM("done")

	env := llmEnv(t, NewLLMBridge(LLMBridgeOptions{Resolver: res, Defaults: LLMRunOptions{Model: "m"}, Source: "test"}))
	_, err := rt.Exec(context.Background(), "stream-finish", `
		var s = llm.stream(null);
		while (s.next()) {} // drain
		var r = s.finish();
		if (r.content !== "done") throw new Error("content: " + r.content);
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
}

func TestLLMBridge_Stream_StartFailureSurfacesAsThrow(t *testing.T) {
	// llm.stream() resolution error must throw to the script (not return a
	// half-built iterator), so scripts can rely on `var s = llm.stream(...)`
	// being a successfully-initialized handle.
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res := &fakeResolver{resolveErr: errors.New("nope")}

	env := llmEnv(t, NewLLMBridge(LLMBridgeOptions{Resolver: res, Defaults: LLMRunOptions{Model: "m"}, Source: "test"}))
	_, err := rt.Exec(context.Background(), "stream-fail", `
		try {
			llm.stream(null);
			throw new Error("expected llm.stream to throw on start failure");
		} catch (e) {
			if (String(e).indexOf("nope") === -1) {
				throw new Error("error should surface resolver message, got: " + e);
			}
		}
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Tool wiring through the facade
// ---------------------------------------------------------------------------

func TestLLMBridge_Run_ToolDefsForwardedToLLM(t *testing.T) {
	rt := jsrt.New(jsrt.WithPoolSize(1))
	res, llmd := newScriptedLLM("ok")

	reg := tool.NewRegistry()
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "search", Description: "s"},
		func(_ context.Context, _ string) (string, error) { return "", nil },
	))
	reg.Register(tool.FuncTool(
		model.ToolDefinition{Name: "calc", Description: "c"},
		func(_ context.Context, _ string) (string, error) { return "", nil },
	))

	bridge := NewLLMBridge(LLMBridgeOptions{
		Resolver: res, Registry: reg,
		Defaults: LLMRunOptions{Model: "m"},
		Source:   "test",
	})
	env := llmEnv(t, bridge)

	// Script restricts to "calc" only; "search" must not reach the LLM.
	_, err := rt.Exec(context.Background(), "run-tools", `
		llm.run({ tools: ["calc"] });
	`, env)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	if llmd.gotOpts == nil {
		t.Fatal("LLM did not receive options")
	}
	if len(llmd.gotOpts.Tools) != 1 || llmd.gotOpts.Tools[0].Name != "calc" {
		t.Errorf("expected exactly tool 'calc' forwarded, got %+v", llmd.gotOpts.Tools)
	}
}
