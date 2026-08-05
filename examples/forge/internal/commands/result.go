package commands

import (
	"github.com/GizClaw/flowcraft/sdk/agent"
)

// resultErrorDetail extracts a human-readable failure reason from a
// non-completed agent result.
func resultErrorDetail(result *agent.Result) string {
	if result == nil {
		return ""
	}
	if result.Err != nil {
		return result.Err.Error()
	}
	if state, ok := result.State["error"].(string); ok && state != "" {
		return state
	}
	if state, ok := result.State["finalize_reason"].(string); ok && state != "" {
		return state
	}
	return ""
}
