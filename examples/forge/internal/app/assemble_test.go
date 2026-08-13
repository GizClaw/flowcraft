package app

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/examples/forge/internal/scenario"
)

func TestOpenBuildsScenarioWorkspaces(t *testing.T) {
	tests := []struct {
		name    string
		kind    string
		source  string
		agentID string
	}{
		{name: "werewolf", kind: "raids", source: "../../scenarios/raids/werewolf", agentID: "assistant"},
		{name: "storyteller", kind: "raids", source: "../../scenarios/raids/multi_role_storyteller", agentID: "assistant"},
		{name: "tom", kind: "personas", source: "../../scenarios/personas/tom", agentID: "tom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, err := scenario.Resolve(tt.kind, tt.source)
			if err != nil {
				t.Fatalf("resolve %s: %v", tt.name, err)
			}
			dir := t.TempDir()
			if err := scenario.Copy(ref, dir); err != nil {
				t.Fatalf("copy scenario: %v", err)
			}
			t.Setenv("DEEPSEEK_API_KEY", "sk-test")

			a, err := Open(context.Background(), dir)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer func() { _ = a.Close() }()
			if a.Info().AgentID != tt.agentID || a.Info().AgentName == "" {
				t.Fatalf("info = %+v", a.Info())
			}
		})
	}
}
