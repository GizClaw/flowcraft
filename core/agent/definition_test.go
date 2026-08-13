package agent_test

import (
	"encoding/json"
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestDefinitionValidate(t *testing.T) {
	def := agent.Definition{
		Card:  agent.AgentCard{Name: "Researcher"},
		Tools: []string{"search"},
		Engine: agent.EngineRef{
			Kind: "graph",
			Deps: resource.Deps{"workspace": "fs"},
		},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("valid definition rejected: %v", err)
	}

	if err := (agent.Definition{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("missing card name error = %v, want validation", err)
	}
	bad := def
	bad.Engine = agent.EngineRef{Deps: resource.Deps{"workspace": "fs"}}
	if err := bad.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("engine without kind error = %v, want validation", err)
	}
}

func TestHookValidate(t *testing.T) {
	if err := (agent.Hook{Type: "recall"}).Validate(); err != nil {
		t.Fatalf("valid hook rejected: %v", err)
	}
	if err := (agent.Hook{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("hook without type error = %v, want validation", err)
	}
	bad := agent.Hook{Type: "recall", Settings: json.RawMessage(`{`)}
	if err := bad.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("bad hook settings error = %v, want validation", err)
	}
}

func TestDefinitionValidatesHooks(t *testing.T) {
	def := agent.Definition{
		Card:    agent.AgentCard{Name: "Researcher"},
		Observe: []agent.Hook{{Type: "audit"}},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("valid hooks rejected: %v", err)
	}
	bad := def
	bad.Observe = []agent.Hook{{}}
	if err := bad.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("hook without type error = %v, want validation", err)
	}
}

func TestAgentCardValidateSkills(t *testing.T) {
	if err := (agent.AgentCard{Name: "A", Skills: []agent.Skill{
		{ID: "search"},
	}}).Validate(); err != nil {
		t.Fatalf("valid card rejected: %v", err)
	}
	bad := agent.AgentCard{Name: "A", Skills: []agent.Skill{{}}}
	if err := bad.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("skill without id error = %v, want validation", err)
	}
}
