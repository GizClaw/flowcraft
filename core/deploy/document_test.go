package deploy_test

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/deploy"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/resource"
)

func TestDocumentValidate(t *testing.T) {
	doc := deploy.Document{
		Version: "v1",
		Resources: resource.Resources{
			"fs": {Kind: "workspace.Registry", Impl: "local"},
		},
		Agents: map[string]agent.Definition{
			"researcher": {
				Card: agent.AgentCard{Name: "Researcher"},
				Engine: agent.EngineRef{
					Kind: "graph",
					Deps: resource.Deps{"workspace": "fs"},
				},
			},
		},
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("valid document rejected: %v", err)
	}

	if err := (deploy.Document{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("missing version error = %v, want validation", err)
	}

	badAgent := doc
	badAgent.Agents = map[string]agent.Definition{
		"x": {Engine: agent.EngineRef{Kind: "graph"}},
	}
	if err := badAgent.Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("agent without card name error = %v, want validation", err)
	}
}

func TestEngineValidate(t *testing.T) {
	if err := (agent.EngineRef{Kind: "graph"}).Validate(); err != nil {
		t.Fatalf("valid engine rejected: %v", err)
	}
	if err := (agent.EngineRef{}).Validate(); !errdefs.IsValidation(err) {
		t.Fatalf("engine without kind error = %v, want validation", err)
	}
}
