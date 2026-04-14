package kanban

// extractPayloadFields extracts query, targetAgentID, and output from a card payload.
// Works with both typed structs (TaskPayload/ResultPayload) and map[string]any.
func extractPayloadFields(payload any) (query, targetAgentID, output string) {
	switch p := payload.(type) {
	case TaskPayload:
		return p.Query, p.TargetAgentID, ""
	case *TaskPayload:
		return p.Query, p.TargetAgentID, ""
	case ResultPayload:
		return "", "", p.Output
	case *ResultPayload:
		return "", "", p.Output
	case map[string]any:
		query, _ = p["query"].(string)
		targetAgentID, _ = p["target_agent_id"].(string)
		output, _ = p["output"].(string)
	}
	return
}

// ExtractPayloadFieldsPublic is a public wrapper around extractPayloadFields.
func ExtractPayloadFieldsPublic(payload any) (query, targetAgentID, output string) {
	return extractPayloadFields(payload)
}

func extractRunID(payload any) string {
	switch p := payload.(type) {
	case map[string]any:
		if id, ok := p["run_id"].(string); ok {
			return id
		}
	}
	return ""
}
