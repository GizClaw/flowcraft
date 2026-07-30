package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// makeAssistantWithToolCall constructs an inference.Message that
// contains one tool-call part — enough to exercise the handoff
// decider.
func makeAssistantWithToolCall(id, name, args string) inference.Message {
	return inference.Message{
		Role: inference.RoleAssistant,
		Content: inference.Content{Parts: []inference.Part{
			inference.ToolCallPart{Call: tool.Call{
				ID:        id,
				Name:      name,
				Arguments: json.RawMessage(args),
			}},
		}},
	}
}

func TestDefaultHandoffToolName(t *testing.T) {
	cases := map[string]string{
		"billing":   "transfer_to_billing",
		"Billing":   "transfer_to_billing",
		"BillSrv":   "transfer_to_billsrv",
		"team-tech": "transfer_to_team_tech",
		"a/b.c":     "transfer_to_a_b_c",
		"":          "transfer_to_unknown",
	}
	for in, want := range cases {
		if got := agent.DefaultHandoffToolName(in); got != want {
			t.Errorf("DefaultHandoffToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHandoffTool_DefinitionShape(t *testing.T) {
	tl := agent.HandoffTool(agent.Handoff{ToAgentID: "billing"})
	def := tl.Definition()
	if def.Name != "transfer_to_billing" {
		t.Fatalf("default name = %q", def.Name)
	}
	if !strings.Contains(def.Description, "billing") {
		t.Fatalf("description must mention target id, got %q", def.Description)
	}
	var schema struct {
		Type                 string         `json:"type"`
		Properties           map[string]any `json:"properties"`
		AdditionalProperties bool           `json:"additionalProperties"`
	}
	if err := json.Unmarshal(def.InputSchema, &schema); err != nil {
		t.Fatalf("input schema must unmarshal: %v", err)
	}
	if schema.Type != "object" {
		t.Fatalf("schema type = %q, want object", schema.Type)
	}
	if len(schema.Properties) != 2 {
		t.Fatalf("schema properties = %v, want reason+note", schema.Properties)
	}
	if schema.AdditionalProperties {
		t.Fatal("schema must be closed (additionalProperties: false)")
	}
}

func TestHandoffTool_PanicsWithoutTarget(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("HandoffTool with empty ToAgentID must panic")
		}
	}()
	_ = agent.HandoffTool(agent.Handoff{})
}

func TestHandoffTool_OnInvokeFires(t *testing.T) {
	var seen agent.HandoffArgs
	tl := agent.HandoffTool(agent.Handoff{
		ToAgentID: "tech",
		OnInvoke: func(_ context.Context, args agent.HandoffArgs) error {
			seen = args
			return nil
		},
	})
	out, err := tl.Execute(context.Background(), `{"reason":"bug","note":"check stack"}`)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if seen.Reason != "bug" || seen.Note != "check stack" {
		t.Fatalf("OnInvoke args = %+v", seen)
	}
	if !strings.Contains(out, "tech") {
		t.Fatalf("tool output should reference target, got %q", out)
	}
}

func TestHandoffTools_RespectsFilter(t *testing.T) {
	hs := []agent.Handoff{
		{ToAgentID: "billing"},
		{
			ToAgentID: "internal",
			Filter:    func(_ context.Context, _ *agent.Request) bool { return false },
		},
		{ToAgentID: "tech"},
	}
	tools := agent.HandoffTools(context.Background(), &agent.Request{}, hs)
	if len(tools) != 2 {
		t.Fatalf("filtered tools = %d, want 2", len(tools))
	}
	names := []string{tools[0].Definition().Name, tools[1].Definition().Name}
	for _, n := range names {
		if strings.Contains(n, "internal") {
			t.Fatalf("filtered handoff still present: %v", names)
		}
	}
}

func TestHandoffDecider_DetectsFirstCall(t *testing.T) {
	hs := []agent.Handoff{
		{ToAgentID: "billing"},
		{ToAgentID: "tech"},
	}
	dec := agent.HandoffDecider(hs)
	res := &agent.Result{
		Messages: []inference.Message{
			makeAssistantWithToolCall("call-1", "transfer_to_billing", `{"reason":"refund"}`),
			makeAssistantWithToolCall("call-2", "transfer_to_tech", ""), // should be ignored
		},
	}
	d, err := dec.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if d.Reason != agent.HandoffFinalizeReason+"billing" {
		t.Fatalf("reason = %q", d.Reason)
	}
	if d.DiscardOutput {
		t.Fatal("default decider must not discard output")
	}

	ev, ok := agent.HandoffFromResult(res)
	if !ok {
		t.Fatal("HandoffFromResult should find an event")
	}
	if ev.ToAgentID != "billing" || ev.ToolCallID != "call-1" || ev.Args.Reason != "refund" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestHandoffDecider_NoMatchReturnsZeroDecision(t *testing.T) {
	dec := agent.HandoffDecider([]agent.Handoff{{ToAgentID: "billing"}})
	res := &agent.Result{
		Messages: []inference.Message{
			makeAssistantWithToolCall("c", "search_kb", "{}"),
		},
	}
	d, err := dec.After(context.Background(), agent.Identity{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("After: %v", err)
	}
	if d != (agent.Decision{}) {
		t.Fatalf("expected zero decision, got %+v", d)
	}
	if _, ok := agent.HandoffFromResult(res); ok {
		t.Fatal("no handoff should be recorded")
	}
}

func TestHandoffDecider_EmptyHandoffsReturnsBaseDecider(t *testing.T) {
	dec := agent.HandoffDecider(nil)
	if _, err := dec.After(context.Background(), agent.Identity{},
		&agent.Request{}, &agent.Result{}); err != nil {
		t.Fatalf("nil-Handoffs decider must be a no-op, err = %v", err)
	}
}

func TestHandoffFromResult_NilSafe(t *testing.T) {
	if _, ok := agent.HandoffFromResult(nil); ok {
		t.Fatal("nil result must not yield a handoff")
	}
	if _, ok := agent.HandoffFromResult(&agent.Result{}); ok {
		t.Fatal("empty state must not yield a handoff")
	}
}
