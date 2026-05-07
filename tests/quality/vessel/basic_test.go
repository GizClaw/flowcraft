package vesselquality

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/vessel"
	"github.com/GizClaw/flowcraft/vessel/spec"

	"github.com/GizClaw/flowcraft/tests/quality/vessel/fakellm"
)

// TestBasicTextReply asserts the simplest happy path: one agent,
// one Generate call, plain text reply lands in Result.Messages.
func TestBasicTextReply(t *testing.T) {
	t.Parallel()
	fake := fakellm.New([]fakellm.Step{
		{Text: "hi there"},
	})
	vs := spec.Spec{
		ID:     "v-basic",
		Agents: []spec.Agent{{Name: "primary"}},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 4)),
	)
	res, err := c.Call(context.Background(), "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "hello"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %s, want StatusOK", res.Status)
	}
	if len(res.Messages) == 0 || !strings.Contains(res.Messages[len(res.Messages)-1].Content(), "hi there") {
		t.Fatalf("messages = %+v, want trailing 'hi there'", res.Messages)
	}
	if calls := fake.Calls(); len(calls) != 1 {
		t.Fatalf("fake.Calls() = %d, want 1", len(calls))
	}
}

// TestToolCallLoop asserts a 2-turn flow: the model first emits a
// tool_call, vessel executes the tool via the registry, the
// result is appended, and a follow-up Generate produces the final
// text reply.
func TestToolCallLoop(t *testing.T) {
	t.Parallel()
	registry := tool.NewRegistry()
	registry.Register(tool.FuncTool(model.ToolDefinition{
		Name:        "echo",
		Description: "Echo input",
		InputSchema: map[string]any{"type": "object"},
	}, echoToolFunc))

	fake := fakellm.New([]fakellm.Step{
		{ToolCalls: []fakellm.Tool{{Name: "echo", Args: `{"text":"x"}`}}},
		{Text: "got: x"},
	})
	vs := spec.Spec{
		ID:     "v-tools",
		Agents: []spec.Agent{{Name: "primary", Tools: []string{"echo"}}},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 4)),
		vessel.WithToolRegistry(registry),
	)
	res, err := c.Call(context.Background(), "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "please echo"),
	})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if res.Status != agent.StatusCompleted {
		t.Fatalf("status = %s", res.Status)
	}
	if !strings.Contains(res.Messages[len(res.Messages)-1].Content(), "got: x") {
		t.Fatalf("final message = %q, want suffix 'got: x'", res.Messages[len(res.Messages)-1].Content())
	}

	// Second Generate call should have seen the tool result.
	calls := fake.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected 2 LLM calls, got %d", len(calls))
	}
	last := calls[1].Messages
	if last[len(last)-1].Role != model.RoleTool {
		t.Fatalf("second turn last message role = %s, want Tool", last[len(last)-1].Role)
	}
}

// TestLLMErrorPropagates asserts a provider error surfaces as a
// non-OK Status (not as a Go error from Call) so callers can
// branch on res.Status without try/catching.
func TestLLMErrorPropagates(t *testing.T) {
	t.Parallel()
	provErr := errdefs.RateLimitf("upstream 429")
	fake := fakellm.New([]fakellm.Step{
		{Err: provErr},
	})
	vs := spec.Spec{
		ID:     "v-err",
		Agents: []spec.Agent{{Name: "primary"}},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 4)),
	)
	res, err := c.Call(context.Background(), "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "ping"),
	})
	if err != nil {
		t.Fatalf("Call returned infra error: %v (expected per-turn Status)", err)
	}
	if res.Status == agent.StatusCompleted {
		t.Fatalf("expected non-OK status; got %s", res.Status)
	}
}

// TestContextCancelDuringSlowGenerate asserts ctx cancellation
// during a slow LLM round-trip aborts the run promptly, and that
// the per-call timeout is honoured to within a small grace.
func TestContextCancelDuringSlowGenerate(t *testing.T) {
	t.Parallel()
	fake := fakellm.New([]fakellm.Step{
		{Delay: 5 * time.Second, Text: "too slow"},
	})
	vs := spec.Spec{
		ID:     "v-cancel",
		Agents: []spec.Agent{{Name: "primary"}},
	}
	c := launchedCaptain(t, vs,
		vessel.WithEngineFactory(fakeLLMEngineFactory(map[string]*fakellm.LLM{"primary": fake}, 4)),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Call(ctx, "primary", agent.Request{
		Message: model.NewTextMessage(model.RoleUser, "wait"),
	})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "context") {
		t.Fatalf("expected ctx error, got %v", err)
	}
	if elapsed > 1*time.Second {
		t.Fatalf("cancel honoured too slowly: %s", elapsed)
	}
}

func echoToolFunc(_ context.Context, args string) (string, error) {
	return "echoed:" + args, nil
}
