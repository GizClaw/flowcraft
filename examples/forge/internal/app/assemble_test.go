package app

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
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
			assertProviders(t, a)
		})
	}
}

// assertProviders pins the provider surface each scenario declares: the
// DeepSeek Responses instance the graphs route to, plus the extra models that
// must stay selectable without a credential being present.
func assertProviders(t *testing.T, a *App) {
	t.Helper()
	value, ok := a.rt.Resource("infer")
	if !ok {
		t.Fatal("no infer resource")
	}
	assembly, ok := value.(*inference.Assembly)
	if !ok {
		t.Fatalf("infer resource is %T", value)
	}
	exposed := make(map[string][]string, len(assembly.Providers()))
	for _, provider := range assembly.Providers() {
		names := make([]string, 0, len(provider.Models))
		for _, model := range provider.Models {
			names = append(names, model.Descriptor.ID.Name)
		}
		sort.Strings(names)
		exposed[provider.ID] = names
	}
	if got := strings.Join(exposed["deepseek"], ","); got != "deepseek-flash" {
		t.Fatalf("deepseek models = %q", got)
	}
	if !contains(exposed["openai"], "gpt-5.6-luna") {
		t.Fatalf("openai models = %v, want gpt-5.6-luna", exposed["openai"])
	}
	if got := strings.Join(exposed["glm"], ","); got != "glm-5.3-flash" {
		t.Fatalf("glm models = %q", got)
	}
	// MiniMax-M3 rides MiniMax's Anthropic-compatible Messages surface, so it
	// is served by the anthropic driver under its own provider id.
	if got := strings.Join(exposed["minimax"], ","); got != "MiniMax-M3" {
		t.Fatalf("minimax models = %q", got)
	}
	// ByteDance keeps its own driver and catalog; the scenario binds the
	// account's dated Ark deployment address in the profile.
	if !contains(exposed["bytedance"], "doubao-seed-2-0-lite") {
		t.Fatalf("bytedance models = %v, want doubao-seed-2-0-lite", exposed["bytedance"])
	}
	// The TUI builds its /model menu from Models(); it must offer exactly the
	// selectable text targets, with automatic routing left to the caller.
	models := a.Models()
	for _, want := range []string{
		"bytedance/doubao-seed-2-0-lite",
		"deepseek/deepseek-flash",
		"glm/glm-5.3-flash",
		"minimax/MiniMax-M3",
		"openai/gpt-5.6-luna",
	} {
		if !contains(models, want) {
			t.Fatalf("Models() = %v, want %s", models, want)
		}
	}
	for _, model := range models {
		if strings.Count(model, "/") != 1 {
			t.Fatalf("Models() entry %q is not provider/name", model)
		}
	}
}

// TestTurnOptionsInputs pins how TUI preferences reach the engine: automatic
// values are omitted so a graph's "${board:x:}" default applies, and explicit
// values ride as board vars the graph references.
func TestTurnOptionsInputs(t *testing.T) {
	if got := (TurnOptions{}).inputs(); got != nil {
		t.Fatalf("auto turn options produced %v", got)
	}
	got := TurnOptions{Model: "glm/glm-5.3-flash", Think: "high"}.inputs()
	if got["model"] != "glm/glm-5.3-flash" || got["think_effort"] != "high" {
		t.Fatalf("inputs = %v", got)
	}
	partial := TurnOptions{Think: "low"}.inputs()
	if _, ok := partial["model"]; ok {
		t.Fatalf("auto model must be omitted: %v", partial)
	}
	if partial["think_effort"] != "low" {
		t.Fatalf("inputs = %v", partial)
	}
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
