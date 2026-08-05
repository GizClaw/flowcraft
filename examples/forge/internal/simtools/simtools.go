// Package simtools registers the demo's application-owned simulated
// tools. The tool implementations are Go values by design: the native
// tool system only declares policy (scopes, middleware) in YAML.
package simtools

import (
	"context"
	"encoding/json"
	"sync/atomic"

	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

// Register registers the func_chat simulated tools and wires the
// shared execution counter used by test statistics.
func Register(registry *tool.Registry, counter *atomic.Int64) {
	if registry == nil {
		return
	}
	registry.Register(&simulatedTool{
		count: counter,
		definition: message.Definition{
			Name:        "play_music",
			Description: "Play a requested song, track, or piece of music.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"title": {"type": "string", "description": "Song or music title to play."},
					"artist": {"type": "string", "description": "Optional artist or composer name."},
					"reason": {"type": "string", "description": "Short reason inferred from the user request."}
				},
				"required": ["title"]
			}`),
		},
	})
	registry.Register(&simulatedTool{
		count: counter,
		definition: message.Definition{
			Name:        "set_device_volume",
			Description: "Set or adjust the current device volume.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"pct": {"type": "number", "description": "Target volume percentage from 0 to 100."},
					"delta": {"type": "number", "description": "Relative volume change, positive to increase and negative to decrease."},
					"reason": {"type": "string", "description": "Short reason inferred from the user request."}
				}
			}`),
		},
	})
	registry.Register(&simulatedTool{
		count: counter,
		definition: message.Definition{
			Name:        "stop_playback",
			Description: "Stop the current music or audio playback.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"reason": {"type": "string", "description": "Short reason inferred from the user request."}
				}
			}`),
		},
	})
	registry.Register(&simulatedTool{
		count: counter,
		definition: message.Definition{
			Name:        "werewolf_game_event",
			Description: "Emit a lifecycle event when Werewolf game state changes.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"event_type": {"type": "string", "description": "setup, night_resolve, game_over, or continue."},
					"phase": {"type": "string", "description": "Current phase after the event."},
					"detail": {"type": "string", "description": "Internal lifecycle detail. Do not use it for public narration."}
				},
				"required": ["event_type", "phase", "detail"]
			}`),
		},
	})
}

type simulatedTool struct {
	definition message.Definition
	count      *atomic.Int64
}

func (t *simulatedTool) Definition() message.Definition {
	return t.definition
}

func (t *simulatedTool) Execute(_ context.Context, arguments string) (string, error) {
	if t.count != nil {
		t.count.Add(1)
	}
	var parsed any
	out := map[string]any{
		"ok":        true,
		"simulated": true,
		"tool":      t.definition.Name,
	}
	if arguments != "" && json.Unmarshal([]byte(arguments), &parsed) == nil {
		out["args"] = parsed
	} else {
		out["args"] = map[string]any{}
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
