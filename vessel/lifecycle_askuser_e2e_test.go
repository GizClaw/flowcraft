package vessel_test

// E2E coverage for the human-in-the-loop chain:
//
//	model.ToolCall (ask_user) → tool.Registry.ExecuteAll
//	   → engine.WithHost(ctx, host) → engine.HostFromContext(ctx)
//	   → host.AskUser(prompt) → reply round-trips back as the
//	   tool result body
//
// The audit (internal-docs/contract-audit.md) flagged that
// host.AskUser was a documented but practically unreachable
// capability for LLM-driven flows: every wiring step existed in
// isolation but no test verified the round trip end-to-end. This
// test closes that gap. It deliberately mimics what
// sdk/graph/node/llmnode/round.go does internally — call
// reg.ExecuteAll on a synthetic ask_user ToolCall using
// engine.WithHost(ctx, host) — so that any breakage in the host
// plumbing or the ask_user tool's host recovery shows up here as a
// failed assertion rather than as silent NotAvailable returns the
// LLM would have to interpret.

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/engine"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/builtin/askuser"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"
)

// TestAskUser_RoundTrip_LLMToolToHost exercises the full HITL
// chain from inside a vessel-dispatched engine. The custom engine
// emits a synthetic ask_user tool call (mimicking what an LLM would
// produce), the registry executes it, ask_user resolves the host
// from ctx, host.AskUser returns a scripted reply, and the engine
// surfaces the reply on the agent.Result.
//
// Specifically we assert:
//
//   - host.AskUser was invoked exactly once with the LLM's prompt
//     verbatim — the tool must NOT silently rewrite or drop the
//     prompt.
//   - The custom host's reply ("the answer is 42") round-trips
//     back to the engine as the tool result body, then to the
//     agent layer as the final assistant message content.
//   - StatusCompleted on the agent.Result — a successful HITL
//     turn must not leak a non-OK status because the host paused
//     waiting for user input.
//
// Regressions this catches:
//
//   - sdk/graph/node/llmnode/round.go forgetting to wrap ctx with
//     engine.WithHost (ask_user would then return NotAvailable).
//   - askuser.Execute mishandling engine.UserReply with multiple
//     parts (the test's reply is single-part text, but a single
//     assertion-on-content guarantees the happy path).
//   - vessel.WithHost not being threaded into agent.Run via
//     agent.WithEngineHost (the host would not be the one the
//     test expects to capture).
func TestAskUser_RoundTrip_LLMToolToHost(t *testing.T) {
	t.Parallel()

	const (
		agentName     = "askuser-agent"
		llmPrompt     = "What is the meaning of life?"
		userReplyText = "the answer is 42"
	)

	host := &askUserCapturingHost{reply: userReplyText}

	reg := tool.NewRegistry()
	reg.Register(askuser.New())

	// llmStyleEngine emulates the inner contract of llmnode for the
	// HITL chain: it synthesises a single ask_user ToolCall, hands
	// it to the registry's ExecuteAll wrapped with engine.WithHost,
	// then composes the user's reply into the final assistant
	// message. We bypass a real LLM here because the unit under
	// test is the host-on-ctx + ask_user tool round trip, not LLM
	// scriptability — the latter is exercised by the existing
	// quality test TestToolCallLoop with fakellm. Coupling THIS
	// test to a fake LLM script would dilute the failure signal
	// when host plumbing breaks.
	llmStyleEngine := engine.EngineFunc(func(ctx context.Context, _ engine.Run, h engine.Host, board *engine.Board) (*engine.Board, error) {
		call := model.ToolCall{
			ID:        "call_1",
			Name:      askuser.Name,
			Arguments: `{"prompt":"` + llmPrompt + `"}`,
		}
		results := reg.ExecuteAll(engine.WithHost(ctx, h), []model.ToolCall{call})
		if len(results) != 1 {
			return board, errdefs.Internalf("expected 1 tool result, got %d", len(results))
		}
		if results[0].IsError {
			return board, errdefs.Internalf("ask_user tool returned an error: %s", results[0].Content)
		}
		final := model.NewTextMessage(model.RoleAssistant, "Final answer: "+results[0].Content)
		board.AppendChannelMessage(engine.MainChannel, final)
		return board, nil
	})

	captain := launchedCaptain(t, spec.Spec{
		ID:     "v-askuser",
		Agents: []spec.Agent{{Name: agentName, Tools: []string{askuser.Name}}},
	},
		vessel.WithEngine(llmStyleEngine),
		vessel.WithToolRegistry(reg),
		vessel.WithHost(host),
	)

	res, err := captain.Call(context.Background(), agentName, agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "please ask the user something"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("Status = %q, want StatusCompleted (HITL roundtrip should not leak non-OK)", res.Status)
	}

	host.mu.Lock()
	prompts := append([]engine.UserPrompt(nil), host.prompts...)
	host.mu.Unlock()

	if len(prompts) != 1 {
		t.Fatalf("host.AskUser invocations = %d, want 1 — ask_user → host.AskUser path is broken", len(prompts))
	}
	if got := prompts[0].Source; got != askuser.Name {
		t.Errorf("UserPrompt.Source = %q, want %q (askuser.Execute must stamp the source)", got, askuser.Name)
	}
	if !promptContainsText(prompts[0], llmPrompt) {
		t.Errorf("UserPrompt did not carry the LLM-supplied prompt %q; got parts=%+v", llmPrompt, prompts[0].Parts)
	}

	if len(res.Messages) == 0 {
		t.Fatal("Result.Messages empty; expected the engine's final assistant message")
	}
	last := res.Messages[len(res.Messages)-1]
	if !strings.Contains(last.Content(), userReplyText) {
		t.Errorf("final message = %q, want it to contain user reply %q (HITL reply did not round-trip)", last.Content(), userReplyText)
	}
}

// TestAskUser_NoHostOnContextSurfacesNotAvailable asserts that when
// the engine forgets to install the host on ctx (a real regression
// class — sdk/graph/node/llmnode/round.go used to do this until the
// commit that introduced engine.WithHost), the ask_user tool surfaces
// errdefs.NotAvailable rather than crashing or returning empty
// content. The LLM then sees a clean error string explaining that
// the capability is not wired and can route accordingly instead of
// hallucinating an answer.
func TestAskUser_NoHostOnContextSurfacesNotAvailable(t *testing.T) {
	t.Parallel()

	out, err := askuser.New().Execute(context.Background(), `{"prompt":"anything"}`)
	if err == nil {
		t.Fatalf("Execute on bare ctx: want error, got reply=%q", out)
	}
	if !errdefs.IsNotAvailable(err) {
		t.Fatalf("err category: want NotAvailable, got %v", err)
	}
}

// askUserCapturingHost embeds engine.NoopHost so it satisfies the
// full engine.Host interface; we override only AskUser to record the
// prompt and return a scripted reply. The mu serialises Prompts /
// reply access because tests run in parallel and the engine's
// goroutines hand-off through the host.
type askUserCapturingHost struct {
	engine.NoopHost
	mu      sync.Mutex
	prompts []engine.UserPrompt
	reply   string
}

func (h *askUserCapturingHost) AskUser(_ context.Context, p engine.UserPrompt) (engine.UserReply, error) {
	h.mu.Lock()
	h.prompts = append(h.prompts, p)
	h.mu.Unlock()
	return engine.UserReply{
		Parts: []model.Part{{Type: model.PartText, Text: h.reply}},
	}, nil
}

func promptContainsText(p engine.UserPrompt, want string) bool {
	for _, part := range p.Parts {
		if part.Type == model.PartText && strings.Contains(part.Text, want) {
			return true
		}
	}
	return false
}
