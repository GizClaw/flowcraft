package template

var builtinTemplates = []GraphTemplate{
	{
		Name:        "blank",
		Label:       "Blank",
		Description: "空白画布，仅包含终止节点，从零开始编排",
		Category:    "basic",
		GraphDef: map[string]any{
			"entry": "",
			"nodes": []map[string]any{},
			"edges": []map[string]any{},
		},
	},
	{
		Name:        "react_agent",
		Label:       "ReAct Agent",
		Description: "LLM → LoopGuard → (条件回跳 LLM) → Answer，支持工具调用",
		Category:    "agent",
		Parameters: []TemplateParameter{
			{Name: "system_prompt", Label: "System Prompt", Type: "textarea", DefaultValue: "You are a helpful AI agent. Use tools when needed."},
			{Name: "max_iterations", Label: "Max Iterations", Type: "number", DefaultValue: 50},
		},
		GraphDef: map[string]any{
			"entry": "llm_call",
			"nodes": []map[string]any{
				{
					"id":   "llm_call",
					"type": "llm",
					"config": map[string]any{
						"system_prompt": "{{.system_prompt}}",
						"track_steps":   true,
						"tool_names": []string{
							"kanban_submit", "task_context",
							"sandbox_bash", "sandbox_read", "sandbox_write",
							"agent", "skill", "fetch_url",
							"knowledge_search", "knowledge_add", "memory_expand",
						},
					},
				},
				{
					"id":   "loop",
					"type": "loopguard",
					"config": map[string]any{
						"max_count": "{{.max_iterations}}",
					},
				},
				{
					"id":     "output",
					"type":   "answer",
					"config": map[string]any{"keys": []string{"response"}},
				},
			},
			"edges": []map[string]any{
				{"from": "llm_call", "to": "loop"},
				{"from": "loop", "to": "llm_call", "condition": "tool_pending == true && loop_count_exceeded == false"},
				{"from": "loop", "to": "output"},
				{"from": "output", "to": "__end__"},
			},
		},
	},
}
