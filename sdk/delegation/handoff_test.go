package delegation_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/delegation"
	"github.com/GizClaw/flowcraft/sdk/message"
)

func assistantToolCall(id, name, args string) message.Message {
	return message.Message{
		Role: message.RoleAssistant,
		Content: message.Content{Parts: []message.Part{
			message.ToolCallPart{Call: message.Call{
				ID:        id,
				Name:      name,
				Arguments: json.RawMessage(args),
			}},
		}},
	}
}

func toolResult(callID string, isError bool) message.Message {
	return message.Message{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.Result{
				CallID:  callID,
				Content: "result",
				IsError: isError,
			}},
		}},
	}
}

func TestHandoffToolUsesUnifiedDelegateShape(t *testing.T) {
	handoffs := []delegation.Handoff{
		{Target: delegation.Target{ID: "billing", Description: "Refunds and invoices"}},
		{Target: delegation.Target{ID: "tech", Description: "Bugs and integrations"}},
	}
	tl := delegation.HandoffTool(context.Background(), &agent.Request{}, handoffs)
	def := tl.Definition()
	if def.Name != delegation.ToolName {
		t.Fatalf("tool name = %q, want %q", def.Name, delegation.ToolName)
	}
	if !strings.Contains(def.Description, "billing") || !strings.Contains(def.Description, "tech") {
		t.Fatalf("description does not list targets: %q", def.Description)
	}

	var schema struct {
		Properties map[string]struct {
			Enum                 []string       `json:"enum"`
			AdditionalProperties map[string]any `json:"additionalProperties"`
		} `json:"properties"`
		Required             []string `json:"required"`
		AdditionalProperties bool     `json:"additionalProperties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if got := schema.Properties["mode"].Enum; len(got) != 1 || got[0] != string(delegation.ModeHandoff) {
		t.Fatalf("mode enum = %v", got)
	}
	if got := schema.Properties["target"].Enum; len(got) != 2 || got[0] != "billing" || got[1] != "tech" {
		t.Fatalf("target enum = %v", got)
	}
	if got := schema.Properties["metadata"].AdditionalProperties["type"]; got != "string" {
		t.Fatalf("metadata additionalProperties.type = %v, want string", got)
	}
	if schema.AdditionalProperties {
		t.Fatal("schema must reject additional properties")
	}
}

func TestHandoffToolFiltersTargetsByMode(t *testing.T) {
	tl := delegation.HandoffTool(context.Background(), &agent.Request{}, []delegation.Handoff{
		{Target: delegation.Target{ID: "all-modes"}},
		{Target: delegation.Target{ID: "handoff", Modes: []delegation.Mode{delegation.ModeHandoff}}},
		{Target: delegation.Target{ID: "sync-only", Modes: []delegation.Mode{delegation.ModeSync}}},
	})

	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tl.Definition().InputSchema, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	got := schema.Properties["target"].Enum
	if len(got) != 2 || got[0] != "all-modes" || got[1] != "handoff" {
		t.Fatalf("target enum = %v, want [all-modes handoff]", got)
	}
}

func TestHandoffToolFiltersTargetsAndInvokesMatchingHook(t *testing.T) {
	var invoked delegation.HandoffArgs
	handoffs := []delegation.Handoff{
		{
			Target: delegation.Target{ID: "hidden"},
			Filter: func(context.Context, *agent.Request) bool { return false },
		},
		{
			Target: delegation.Target{ID: "tech"},
			OnInvoke: func(_ context.Context, args delegation.HandoffArgs) error {
				invoked = args
				return nil
			},
		},
	}
	tl := delegation.HandoffTool(context.Background(), &agent.Request{}, handoffs)
	out, err := tl.Execute(context.Background(), `{"mode":"handoff","target":"tech","input":"fix it","note":"stack trace attached","metadata":{"priority":"high"}}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if invoked.Target != "tech" || invoked.Input != "fix it" || invoked.Note != "stack trace attached" {
		t.Fatalf("OnInvoke args = %+v", invoked)
	}
	if invoked.Metadata["priority"] != "high" {
		t.Fatalf("OnInvoke metadata = %v", invoked.Metadata)
	}
	if !strings.Contains(out, "tech") {
		t.Fatalf("output = %q", out)
	}
}

func TestHandoffToolAppliesFilterToNilRequest(t *testing.T) {
	var sawNil bool
	tl := delegation.HandoffTool(context.Background(), nil, []delegation.Handoff{
		{
			Target: delegation.Target{ID: "hidden"},
			Filter: func(_ context.Context, req *agent.Request) bool {
				sawNil = req == nil
				return false
			},
		},
		{Target: delegation.Target{ID: "visible"}},
	})
	if !sawNil {
		t.Fatal("Filter was not called with nil request")
	}

	var schema struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tl.Definition().InputSchema, &schema); err != nil {
		t.Fatalf("schema: %v", err)
	}
	if got := schema.Properties["target"].Enum; len(got) != 1 || got[0] != "visible" {
		t.Fatalf("target enum = %v, want [visible]", got)
	}
}

func TestHandoffToolStrictlyDecodesArguments(t *testing.T) {
	tl := delegation.HandoffTool(context.Background(), &agent.Request{}, []delegation.Handoff{
		{Target: delegation.Target{ID: "tech"}},
	})
	for _, raw := range []string{
		`{"mode":"handoff","target":"tech","input":"fix","extra":true}`,
		`{"mode":"handoff","target":"tech","input":"fix"} {}`,
		`{"mode":"handoff","target":"tech","input":"fix","metadata":{"priority":1}}`,
	} {
		if _, err := tl.Execute(context.Background(), raw); err == nil {
			t.Fatalf("Execute(%q) succeeded, want strict decode error", raw)
		}
	}
}

func TestHandoffRefereeDetectsFirstMatchingCall(t *testing.T) {
	ref := delegation.HandoffReferee([]delegation.Handoff{
		{Target: delegation.Target{ID: "billing"}},
		{Target: delegation.Target{ID: "tech"}},
	})
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("ignore-tool", "search", `{"query":"refund"}`),
		assistantToolCall("ignore-mode", delegation.ToolName, `{"mode":"sync","target":"billing","input":"refund"}`),
		assistantToolCall("first", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund"}`),
		assistantToolCall("second", delegation.ToolName, `{"mode":"handoff","target":"tech","input":"bug"}`),
		toolResult("ignore-mode", false),
		toolResult("first", false),
		toolResult("second", false),
	}}

	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision.Reason != delegation.HandoffFinalizeReason+"billing" {
		t.Fatalf("reason = %q", decision.Reason)
	}
	event, ok := delegation.HandoffFromResult(res)
	if !ok {
		t.Fatal("handoff event not found")
	}
	if event.Target != "billing" || event.ToolCallID != "first" || event.Args.Input != "refund" {
		t.Fatalf("event = %+v", event)
	}
}

func TestHandoffRefereeIgnoresMalformedAndUnknownTargets(t *testing.T) {
	ref := delegation.HandoffReferee([]delegation.Handoff{
		{Target: delegation.Target{ID: "billing"}},
	})
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("bad-json", delegation.ToolName, `{`),
		assistantToolCall("unknown", delegation.ToolName, `{"mode":"handoff","target":"tech","input":"bug"}`),
		toolResult("bad-json", false),
		toolResult("unknown", false),
	}}
	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision != (agent.Decision{}) {
		t.Fatalf("decision = %+v, want zero", decision)
	}
	if _, ok := delegation.HandoffFromResult(res); ok {
		t.Fatal("unexpected handoff event")
	}
}

func TestDirectoryHandoffRefereeResolvesTargetsAtDecisionTime(t *testing.T) {
	directory := &mutableDirectory{}
	ref := delegation.DirectoryHandoffReferee(directory)
	directory.targets = map[string]delegation.Target{
		"billing": {
			ID:    "billing",
			Modes: []delegation.Mode{delegation.ModeHandoff},
		},
	}
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("unknown", delegation.ToolName, `{"mode":"handoff","target":"tech","input":"bug"}`),
		assistantToolCall("first", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund"}`),
		assistantToolCall("second", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"duplicate"}`),
		toolResult("unknown", false),
		toolResult("first", false),
		toolResult("second", false),
	}}

	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision.Reason != delegation.HandoffFinalizeReason+"billing" {
		t.Fatalf("reason = %q", decision.Reason)
	}
	event, ok := delegation.HandoffFromResult(res)
	if !ok || event.ToolCallID != "first" {
		t.Fatalf("event = %+v, found = %v", event, ok)
	}
}

func TestDirectoryHandoffRefereeRequiresStrictValidHandoff(t *testing.T) {
	directory := &mutableDirectory{targets: map[string]delegation.Target{
		"sync-only": {ID: "sync-only", Modes: []delegation.Mode{delegation.ModeSync}},
		"billing":   {ID: "billing", Modes: []delegation.Mode{delegation.ModeHandoff}},
	}}
	ref := delegation.DirectoryHandoffReferee(directory)
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("extra", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund","extra":true}`),
		assistantToolCall("empty", delegation.ToolName, `{"mode":"handoff","target":"billing","input":""}`),
		assistantToolCall("unsupported", delegation.ToolName, `{"mode":"handoff","target":"sync-only","input":"refund"}`),
		toolResult("extra", false),
		toolResult("empty", false),
		toolResult("unsupported", false),
	}}

	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision != (agent.Decision{}) {
		t.Fatalf("decision = %+v, want zero", decision)
	}
}

func TestHandoffRefereeRequiresSuccessfulMatchingToolResult(t *testing.T) {
	ref := delegation.HandoffReferee([]delegation.Handoff{
		{Target: delegation.Target{ID: "billing"}},
	})
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("failed", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund"}`),
		toolResult("failed", true),
		assistantToolCall("missing", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund"}`),
		toolResult("other", false),
	}}

	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision != (agent.Decision{}) {
		t.Fatalf("decision = %+v, want zero", decision)
	}
}

func TestHandoffRefereeRecomputesStaticFilterForRequest(t *testing.T) {
	ref := delegation.HandoffReferee([]delegation.Handoff{{
		Target: delegation.Target{ID: "billing"},
		Filter: func(_ context.Context, req *agent.Request) bool {
			return req != nil && req.ContextID == "allowed"
		},
	}})
	newResult := func() *agent.Result {
		return &agent.Result{Messages: []message.Message{
			assistantToolCall("call", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund"}`),
			toolResult("call", false),
		}}
	}

	denied := newResult()
	decision, err := ref.After(context.Background(), agent.Identity{}, nil, denied)
	if err != nil {
		t.Fatalf("After nil: %v", err)
	}
	if decision != (agent.Decision{}) {
		t.Fatalf("nil decision = %+v, want zero", decision)
	}

	allowed := newResult()
	decision, err = ref.After(context.Background(), agent.Identity{}, &agent.Request{ContextID: "allowed"}, allowed)
	if err != nil {
		t.Fatalf("After allowed: %v", err)
	}
	if decision.Reason != delegation.HandoffFinalizeReason+"billing" {
		t.Fatalf("allowed reason = %q", decision.Reason)
	}
}

func TestHandoffRefereeStaticBranchRequiresStrictValidSupportedHandoff(t *testing.T) {
	ref := delegation.HandoffReferee([]delegation.Handoff{
		{Target: delegation.Target{ID: "sync-only", Modes: []delegation.Mode{delegation.ModeSync}}},
		{Target: delegation.Target{ID: "billing", Modes: []delegation.Mode{delegation.ModeHandoff}}},
	})
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("extra", delegation.ToolName, `{"mode":"handoff","target":"billing","input":"refund","extra":true}`),
		assistantToolCall("empty", delegation.ToolName, `{"mode":"handoff","target":"billing","input":""}`),
		assistantToolCall("unsupported", delegation.ToolName, `{"mode":"handoff","target":"sync-only","input":"refund"}`),
		toolResult("extra", false),
		toolResult("empty", false),
		toolResult("unsupported", false),
	}}
	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision != (agent.Decision{}) {
		t.Fatalf("decision = %+v, want zero", decision)
	}
}

func TestHandoffRefereeIgnoresToolExecutionFailures(t *testing.T) {
	invokeErr := errors.New("invoke failed")
	called := 0
	handoff := delegation.Handoff{
		Target: delegation.Target{ID: "billing"},
		OnInvoke: func(context.Context, delegation.HandoffArgs) error {
			called++
			return invokeErr
		},
	}
	tl := delegation.HandoffTool(context.Background(), &agent.Request{}, []delegation.Handoff{handoff})
	raw := `{"mode":"handoff","target":"billing","input":"refund"}`
	if _, err := tl.Execute(context.Background(), raw); !errors.Is(err, invokeErr) {
		t.Fatalf("Execute OnInvoke error = %v, want %v", err, invokeErr)
	}
	if _, err := tl.Execute(context.Background(), raw+` {}`); err == nil {
		t.Fatal("Execute trailing JSON succeeded")
	}
	if called != 1 {
		t.Fatalf("OnInvoke calls = %d, want 1", called)
	}

	ref := delegation.HandoffReferee([]delegation.Handoff{handoff})
	res := &agent.Result{Messages: []message.Message{
		assistantToolCall("invoke-error", delegation.ToolName, raw),
		toolResult("invoke-error", true),
		assistantToolCall("parse-error", delegation.ToolName, raw+` {}`),
		toolResult("parse-error", true),
	}}
	decision, err := ref.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if decision != (agent.Decision{}) {
		t.Fatalf("decision = %+v, want zero", decision)
	}
}

func TestHandoffFromResultNilSafe(t *testing.T) {
	if _, ok := delegation.HandoffFromResult(nil); ok {
		t.Fatal("nil result yielded an event")
	}
	if _, ok := delegation.HandoffFromResult(&agent.Result{}); ok {
		t.Fatal("empty result yielded an event")
	}
}

type mutableDirectory struct {
	targets map[string]delegation.Target
}

func (d *mutableDirectory) List(context.Context) ([]delegation.Target, error) {
	return nil, nil
}

func (d *mutableDirectory) Get(_ context.Context, id string) (delegation.Target, error) {
	target, ok := d.targets[id]
	if !ok {
		return delegation.Target{}, delegation.TargetNotFound(id)
	}
	return target, nil
}
