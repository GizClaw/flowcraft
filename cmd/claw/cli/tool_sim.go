package cli

import (
	"context"
	"encoding/json"

	"github.com/GizClaw/flowcraft/sdkx/claw"
)

func attachSimulatedToolHandler(app *claw.Claw) {
	if app == nil {
		return
	}
	app.HandleDefault(func(_ context.Context, name string, args json.RawMessage) (string, error) {
		var parsed any
		out := map[string]any{
			"ok":        true,
			"simulated": true,
			"tool":      name,
		}
		if len(args) > 0 && json.Unmarshal(args, &parsed) == nil {
			out["args"] = parsed
		} else {
			out["args"] = map[string]any{}
		}
		raw, err := json.Marshal(out)
		if err != nil {
			return "", err
		}
		return string(raw), nil
	})
}
