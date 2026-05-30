package history

import "github.com/GizClaw/flowcraft/sdk/model"

// sanitizeToolPairs removes orphaned tool_result messages that can appear
// after truncation. Anthropic (and compatible APIs) require every tool_result
// to reference a preceding tool_use in the same conversation window.
// Violating this causes a 400 "tool id not found" error.
//
// Only tool_result messages with missing tool_use counterparts are removed.
// Assistant messages with tool_use are always preserved — the trailing
// tool_use without a result is legitimate (the tool may still be pending).
func sanitizeToolPairs(msgs []model.Message) []model.Message {
	toolUseIDs := make(map[string]bool)
	for _, m := range msgs {
		if m.Role == model.RoleAssistant {
			for _, tc := range m.ToolCalls() {
				toolUseIDs[tc.ID] = true
			}
		}
	}

	out := make([]model.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == model.RoleTool {
			results := m.ToolResults()
			if len(results) == 0 {
				out = append(out, m)
				continue
			}
			allFound := true
			for _, r := range results {
				if !toolUseIDs[r.ToolCallID] {
					allFound = false
					break
				}
			}
			if !allFound {
				continue
			}
		}
		out = append(out, m)
	}

	return out
}
