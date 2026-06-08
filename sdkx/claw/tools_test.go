package claw

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/model"
)

func TestToolConfigsAcceptStringsAndSchemaObjects(t *testing.T) {
	raw := []byte(`[
		"play_music",
		{
			"name": "set_device_volume",
			"description": "Set volume",
			"input_schema": {
				"type": "object",
				"properties": {
					"pct": {"type": "number"}
				},
				"required": ["pct"]
			}
		}
	]`)
	var tools ToolConfigs
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got := strings.Join(tools.Names(), ","); got != "play_music,set_device_volume" {
		t.Fatalf("Names = %q", got)
	}
	if tools[1].InputSchema["type"] != "object" {
		t.Fatalf("schema lost: %+v", tools[1].InputSchema)
	}
}

func TestToolRegistryUsesHandleDefaultAndIgnoresMissingHandler(t *testing.T) {
	app := &Claw{
		cfg: Config{Agent: AgentConfig{Tools: ToolConfigs{
			{Name: "play_music"},
			{Name: "set_device_volume"},
		}}},
	}
	reg := app.buildToolRegistry()
	app.HandleDefault(func(_ context.Context, name string, args json.RawMessage) (string, error) {
		return `{"handled":"` + name + `","args":` + string(args) + `}`, nil
	})

	res := reg.Execute(context.Background(), model.ToolCall{ID: "call_1", Name: "play_music", Arguments: `{"title":"卡农"}`})
	if res.IsError || !strings.Contains(res.Content, `"handled":"play_music"`) {
		t.Fatalf("default handler result = %+v", res)
	}

	app.HandleDefault(nil)
	res = reg.Execute(context.Background(), model.ToolCall{ID: "call_2", Name: "set_device_volume", Arguments: `{}`})
	if res.IsError || !strings.Contains(res.Content, `"ignored":true`) {
		t.Fatalf("ignored result = %+v", res)
	}
}
