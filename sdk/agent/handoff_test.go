package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/model"
)

// makeAssistantWithToolCall constructs a model.Message that contains
// one tool-call part — enough to exercise the handoff decider.
func makeAssistantWithToolCall(id, name, args string) model.Message {
	return model.Message{
		Role: model.RoleAssistant,
		Parts: []model.Part{
			{
				Type: model.PartToolCall,
				ToolCall: &model.ToolCall{
					ID:        id,
					Name:      name,
					Arguments: args,
				},
			},
		},
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
	if def.InputSchema["type"] != "object" {
		t.Fatalf("schema type = %v", def.InputSchema["type"])
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
		Messages: []model.Message{
			makeAssistantWithToolCall("call-1", "transfer_to_billing", `{"reason":"refund"}`),
			makeAssistantWithToolCall("call-2", "transfer_to_tech", ""), // should be ignored
		},
	}
	d, err := dec.BeforeFinalize(context.Background(), agent.RunInfo{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("BeforeFinalize: %v", err)
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
		Messages: []model.Message{
			makeAssistantWithToolCall("c", "search_kb", "{}"),
		},
	}
	d, err := dec.BeforeFinalize(context.Background(), agent.RunInfo{}, &agent.Request{}, res)
	if err != nil {
		t.Fatalf("BeforeFinalize: %v", err)
	}
	if d != (agent.FinalizeDecision{}) {
		t.Fatalf("expected zero decision, got %+v", d)
	}
	if _, ok := agent.HandoffFromResult(res); ok {
		t.Fatal("no handoff should be recorded")
	}
}

func TestHandoffDecider_EmptyHandoffsReturnsBaseDecider(t *testing.T) {
	dec := agent.HandoffDecider(nil)
	if _, err := dec.BeforeFinalize(context.Background(), agent.RunInfo{},
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
