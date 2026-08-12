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
		Card:  agent.Card{Name: "Researcher"},
		Tools: []string{"search"},
		Engine: agent.Engine{
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
	bad.Engine = agent.Engine{Deps: resource.Deps{"workspace": "fs"}}
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
		Card: agent.Card{Name: "Researcher"},
		Hooks: map[string][]agent.Hook{
			"observe": {{Type: "audit"}},
		},
	}
	if err := def.Validate(); err != nil {
		t.Fatalf("valid hooks rejected: %v", err)
	}
	bad := def
	bad.Hooks = map[string][]agent.Hook{"observe": {{}}}
	if err := bad.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("hook without type error = %v, want validation", err)
	}
}
