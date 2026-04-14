package node

func init() {
	for _, s := range builtinSchemas {
		RegisterDefaultSchema(s)
	}
}

var builtinSchemas = []NodeSchema{
	{
		Type: "llm", Label: "LLM", Icon: "Brain", Color: "blue", Category: "general",
		Description: "Call a large language model to generate text",
		Fields: []FieldSchema{
			{Key: "system_prompt", Label: "System Prompt", Type: "textarea"},
			{Key: "model", Label: "Model (leave empty to use global default; format: provider/model, e.g. openai/gpt-4o; use schema(action=model_list) to see available options)", Type: "select", Placeholder: "Use Default Model"},
			{Key: "temperature", Label: "Temperature", Type: "number", DefaultValue: 1.0},
			{Key: "max_tokens", Label: "Max Tokens", Type: "number"},
			{Key: "json_mode", Label: "JSON Mode", Type: "boolean", DefaultValue: false},
			{Key: "query_fallback", Label: "Query Fallback", Type: "boolean", DefaultValue: false},
			{Key: "track_steps", Label: "Track Steps", Type: "boolean", DefaultValue: false},
			{Key: "output_key", Label: "Output Key", Type: "text", DefaultValue: "response"},
			{Key: "messages_key", Label: "Messages Key", Type: "text", DefaultValue: "messages"},
			{Key: "tool_names", Label: "Tool Names (JSON array of tool name strings; use schema(action=tool_list) to see available tools; empty = no tool-calling)", Type: "json"},
		},
		InputPorts: []PortSchema{
			{Name: "messages", Type: "messages", Required: true},
		},
		OutputPorts: []PortSchema{
			{Name: "response", Type: "string", Required: true},
			{Name: "messages", Type: "messages", Required: true},
			{Name: "usage", Type: "usage", Required: true},
			{Name: "tool_pending", Type: "bool", Required: true},
		},
		Runtime: &RuntimeSpec{
			BoardWrites: []BoardVarSpec{
				{Key: "${output_key}", Type: "string", Desc: "LLM text output, stored under the configured output_key (default: response)"},
				{Key: "${output_key}", Type: "map", Desc: "When json_mode=true, JSON output is parsed into a map object. Condition edges MUST use dot-notation to access nested fields: e.g. if output_key='intent_result', use intent_result.intent == 'value'", Condition: "json_mode == true"},
				{Key: "${messages_key}", Type: "messages", Desc: "Updated conversation history (default key: messages)"},
				{Key: "tool_pending", Type: "bool", Desc: "true if the LLM made tool calls requiring another iteration"},
			},
			EdgeVars: []BoardVarSpec{
				{Key: "tool_pending", Type: "bool", Desc: "Use in condition edges to loop back for tool execution: tool_pending == true"},
				{Key: "${output_key}", Type: "string", Desc: "Direct string comparison when json_mode=false: output_key == 'value'"},
				{Key: "${output_key}.<field>", Type: "any", Desc: "Dot-notation field access when json_mode=true: output_key.field == 'value'", Condition: "json_mode == true"},
			},
			Notes: []string{
				"CRITICAL: When json_mode=true, output_key stores a PARSED MAP, not a string. Condition edges must use dot-notation to access fields inside: e.g. intent_result.intent == 'qa' (NOT intent == 'qa')",
				"Multiple LLM nodes sharing the same messages_key will pollute each other's system_prompt. Use different messages_key for each LLM node with a distinct system_prompt",
				"Nodes with non-default messages_key MUST set query_fallback=true, otherwise the isolated message list starts empty and the LLM receives no user input",
				"When json_mode=false and system_prompt instructs the LLM to output a single keyword, output_key stores that keyword as a plain string — condition edges can compare directly: output_key == 'keyword'",
				"tool_names is an array of tool name strings. When empty, the LLM has NO tool-calling ability. Use schema(action=tool_list) to see all available tools, then list the ones this node needs. Example: [\"knowledge_search\", \"fetch_url\"]",
				"For ReAct agent patterns, the LLM node MUST have tool_names configured and the graph must include a loopguard + conditional loop back on tool_pending == true",
				"When lossless memory is enabled, summary index is auto-injected into system prompt. Add memory_expand to tool_names so the LLM can expand compressed summaries to see original messages on demand.",
			},
		},
	},
	{
		Type: "router", Label: "Router", Icon: "GitBranch", Color: "purple", Category: "general",
		Description: "Conditional routing based on expression evaluation",
		Fields: []FieldSchema{
			{Key: "routes", Label: "Routes", Type: "json", Required: true},
		},
		InputPorts:  []PortSchema{{Name: "routes", Type: "any", Required: true}},
		OutputPorts: []PortSchema{{Name: "route_target", Type: "string", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "(all board vars)", Type: "any", Desc: "Route conditions are evaluated against all current board variables via expr"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "route_target", Type: "string", Desc: "The target node ID of the first matching route"},
			},
			Notes: []string{
				"Routes are evaluated in order; first match wins",
				"A route without a condition acts as the default fallback — only reached when no other route matches",
				"If no route matches and no default exists, route_target is set to empty string",
				"Route condition expressions reference board variables directly: e.g. intent == 'qa' requires a board var named 'intent'",
			},
		},
	},
	{
		Type: "ifelse", Label: "If/Else", Icon: "GitFork", Color: "purple", Category: "general",
		Description: "Multi-branch conditional evaluation",
		Fields: []FieldSchema{
			{Key: "conditions", Label: "Conditions", Type: "json", Required: true},
		},
		InputPorts:  []PortSchema{{Name: "conditions", Type: "any", Required: true}},
		OutputPorts: []PortSchema{{Name: "branch_result", Type: "string", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "(all board vars)", Type: "any", Desc: "Condition expressions are evaluated against all current board variables via expr"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "branch_result", Type: "string", Desc: "Result label: 'if' (index 0 matched), 'elif_1', 'elif_2', ..., or 'else' (none matched)"},
			},
			EdgeVars: []BoardVarSpec{
				{Key: "branch_result", Type: "string", Desc: "Use in condition edges: branch_result == 'if', branch_result == 'elif_1', branch_result == 'else'"},
			},
			Notes: []string{
				"Conditions are index-based: index 0 → 'if', index 1 → 'elif_1', index 2 → 'elif_2', etc.",
				"If no condition matches, branch_result is 'else'",
				"Downstream condition edges must use these exact string values",
			},
		},
	},
	{
		Type: "template", Label: "Template", Icon: "FileText", Color: "green", Category: "general",
		Description: "Render text using Go template syntax",
		Fields: []FieldSchema{
			{Key: "template", Label: "Template", Type: "textarea", Required: true, Placeholder: "Hello, {{.name}}!"},
		},
		InputPorts:  []PortSchema{{Name: "template", Type: "string", Required: true}},
		OutputPorts: []PortSchema{{Name: "template_output", Type: "string", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "(all board vars)", Type: "any", Desc: "All board variables are available as {{.varName}} placeholders in the template"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "template_output", Type: "string", Desc: "The rendered template string with all placeholders replaced"},
			},
			Notes: []string{
				"Uses {{.varName}} syntax for placeholder substitution (simple string replacement, not full Go template engine)",
				"Only top-level board variables are accessible — nested access like {{.obj.field}} is NOT supported",
				"Null/undefined variables are replaced with empty string",
			},
		},
	},
	{
		Type: "answer", Label: "Answer", Icon: "MessageSquare", Color: "green", Category: "general",
		Description: "Extract final output from the board",
		Fields: []FieldSchema{
			{Key: "template", Label: "Template", Type: "textarea"},
			{Key: "keys", Label: "Keys", Type: "json", DefaultValue: []string{"response"}},
			{Key: "stream", Label: "Stream Output", Type: "boolean", DefaultValue: true},
		},
		InputPorts:  []PortSchema{{Name: "template", Type: "string"}, {Name: "keys", Type: "any"}},
		OutputPorts: []PortSchema{{Name: "answer", Type: "string", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "${keys[*]}", Type: "any", Desc: "Reads values from each key listed in config.keys (default: ['response'])"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "answer", Type: "string", Desc: "The final answer string (concatenation of keys values or rendered template)"},
			},
			Notes: []string{
				"Two modes: (1) if template is set, renders {{.varName}} placeholders; (2) otherwise, concatenates values from keys with newline separator",
				"Streaming is ON by default — set stream=false to suppress SSE output",
				"The keys config must reference the correct upstream output_key values: e.g. if LLM uses output_key='analysis_result', keys should include 'analysis_result'",
				"Empty/null/undefined values from keys are silently dropped in concatenation mode",
			},
		},
	},
	{
		Type: "assigner", Label: "Assigner", Icon: "ArrowRight", Color: "gray", Category: "general",
		Description: "Assign values to board variables",
		Fields: []FieldSchema{
			{Key: "assignments", Label: "Assignments", Type: "json", Required: true},
		},
		InputPorts: []PortSchema{{Name: "assignments", Type: "any", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "${assignment.source}", Type: "any", Desc: "When assignment uses 'source', reads from the named board variable"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "${assignment.target}", Type: "any", Desc: "Writes to the target board variable specified in each assignment"},
			},
			Notes: []string{
				"Each assignment supports three value sources (priority: source > value > expression): 'source' copies from another var, 'value' uses a literal, 'expression' evaluates an expr",
				"Target supports dot-notation for nested writes: e.g. target='obj.nested.field' creates intermediate objects automatically",
				"Useful for extracting fields from a json_mode LLM output map and promoting them to top-level board vars for simpler condition edges",
			},
		},
	},
	{
		Type: "loopguard", Label: "Loop Guard", Icon: "RefreshCw", Color: "orange", Category: "general",
		Description: "Loop counter guard to prevent infinite loops",
		Fields: []FieldSchema{
			{Key: "max_count", Label: "Max Count", Type: "number", DefaultValue: 10},
			{Key: "counter_key", Label: "Counter Key", Type: "text", DefaultValue: "__loop_count"},
		},
		OutputPorts: []PortSchema{
			{Name: "loop_count", Type: "integer", Required: true},
			{Name: "loop_count_exceeded", Type: "bool", Required: true},
		},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "${counter_key}", Type: "int", Desc: "Reads current counter value (default key: __loop_count)"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "${counter_key}", Type: "int", Desc: "Incremented counter value (default key: __loop_count)"},
				{Key: "loop_count", Type: "int", Desc: "Copy of current counter value for display/logging"},
				{Key: "loop_count_exceeded", Type: "bool", Desc: "true when count >= max_count"},
			},
			EdgeVars: []BoardVarSpec{
				{Key: "loop_count_exceeded", Type: "bool", Desc: "Use in condition edges: loop_count_exceeded == false (continue loop) or loop_count_exceeded == true (exit loop)"},
			},
			Notes: []string{
				"Counter increments BEFORE the check — first execution sets count to 1",
				"With max_count=10, loop_count_exceeded becomes true on the 10th iteration (uses >= comparison)",
				"Typical ReAct pattern: condition edge 'tool_pending == true && loop_count_exceeded == false' to loop back",
			},
		},
	},
	{
		Type: "aggregator", Label: "Aggregator", Icon: "Layers", Color: "teal", Category: "general",
		Description: "Aggregate multiple inputs into a single output",
		Fields: []FieldSchema{
			{Key: "input_keys", Label: "Input Keys", Type: "json", Required: true},
			{
				Key: "mode", Label: "Mode", Type: "select", DefaultValue: "array",
				Options: []SelectOption{
					{Value: "array", Label: "Array"},
					{Value: "concat", Label: "Concat"},
					{Value: "map", Label: "Map"},
					{Value: "last", Label: "Last"},
				},
			},
			{Key: "output_key", Label: "Output Key", Type: "text", DefaultValue: "aggregated"},
			{Key: "separator", Label: "Separator", Type: "text", DefaultValue: "\n"},
		},
		InputPorts:  []PortSchema{{Name: "input_keys", Type: "any", Required: true}},
		OutputPorts: []PortSchema{{Name: "aggregated", Type: "any"}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "${input_keys[*]}", Type: "any", Desc: "Reads values from each key listed in input_keys"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "${output_key}", Type: "any", Desc: "Aggregated result stored under output_key (default: aggregated)"},
			},
			Notes: []string{
				"ONLY 4 modes are supported: array, concat, map, last. Any other value (e.g. 'first_not_empty') silently falls back to 'array'",
				"array: collects non-undefined values into a list",
				"concat: joins values as strings with separator (default: newline)",
				"map: creates {key: value} object preserving all keys (including undefined ones)",
				"last: returns only the last non-undefined value, or null if all undefined",
				"input_keys must match the exact board variable names written by upstream nodes (e.g. the output_key of LLM nodes)",
			},
		},
	},
	{
		Type: "gate", Label: "Gate", Icon: "Shield", Color: "red", Category: "special",
		Description: "Shell command validation gate",
		Fields: []FieldSchema{
			{Key: "commands", Label: "Commands", Type: "json", Required: true},
		},
		InputPorts: []PortSchema{{Name: "commands", Type: "any", Required: true}},
		OutputPorts: []PortSchema{
			{Name: "gate_result", Type: "string", Required: true},
			{Name: "gate_result_output", Type: "string", Required: true},
		},
		Runtime: &RuntimeSpec{
			BoardWrites: []BoardVarSpec{
				{Key: "gate_result", Type: "string", Desc: "'pass' if all commands succeeded, 'fail' if any command returned non-zero exit code"},
				{Key: "gate_result_output", Type: "string", Desc: "Accumulated stdout from all executed commands"},
				{Key: "gate_result_failed_command", Type: "string", Desc: "The command string that failed (only set on failure)"},
			},
			EdgeVars: []BoardVarSpec{
				{Key: "gate_result", Type: "string", Desc: "Use in condition edges: gate_result == 'pass' or gate_result == 'fail'"},
			},
			Notes: []string{
				"Commands run sequentially; first failure aborts remaining commands",
				"On failure, the gate calls signal.done() which TERMINATES graph execution — downstream nodes are NOT reached",
				"Requires a sandbox/shell bridge to be configured",
			},
		},
	},
	{
		Type: "context", Label: "Context", Icon: "FileInput", Color: "green", Category: "special",
		Description: "Load context from files or shell commands",
		Fields: []FieldSchema{
			{Key: "files", Label: "Files", Type: "json"},
			{Key: "commands", Label: "Commands", Type: "json"},
		},
		InputPorts: []PortSchema{{Name: "files", Type: "any"}, {Name: "commands", Type: "any"}},
		Runtime: &RuntimeSpec{
			BoardWrites: []BoardVarSpec{
				{Key: "${file.state_key || file.path}", Type: "string", Desc: "File content stored under state_key (or file path if state_key not set)"},
				{Key: "${cmd.state_key || cmd.command}", Type: "string", Desc: "Command stdout stored under state_key (or raw command string if state_key not set)"},
			},
			Notes: []string{
				"Always set state_key for predictable board variable names — without it, the file path or raw command string becomes the key",
				"Only stdout is captured from commands; stderr is discarded",
				"Requires sandbox/shell and fs bridges to be configured",
			},
		},
	},
	{
		Type: "approval", Label: "Approval", Icon: "CheckCircle", Color: "yellow", Category: "special",
		Description: "Human approval checkpoint",
		Fields: []FieldSchema{
			{Key: "prompt", Label: "Approval Prompt", Type: "textarea", DefaultValue: "Please approve or reject."},
		},
		InputPorts:  []PortSchema{{Name: "prompt", Type: "string"}},
		OutputPorts: []PortSchema{{Name: "approval_status", Type: "string", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "approval_decision", Type: "string", Desc: "Set externally by the approval UI/API to resume execution"},
				{Key: "approval_status", Type: "string", Desc: "Current approval state"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "approval_status", Type: "string", Desc: "'pending' on first invocation, then the decision value on resume"},
				{Key: "approval_request", Type: "object", Desc: "Contains {prompt, node_id} — emitted on first invocation to trigger the approval UI"},
			},
			EdgeVars: []BoardVarSpec{
				{Key: "approval_status", Type: "string", Desc: "Use in condition edges: approval_status == 'approved' or approval_status == 'rejected'"},
			},
			Notes: []string{
				"Two-phase node: first invocation sets status to 'pending' and calls signal.interrupt() to pause the graph",
				"Execution resumes when approval_decision is set externally via the resume API, then the node copies it to approval_status",
				"If re-invoked while status is already 'pending' and no decision is set, the node does nothing (no-op)",
			},
		},
	},
	{
		Type: "iteration", Label: "Iteration", Icon: "Repeat", Color: "indigo", Category: "special",
		Description: "Iterate over a list and execute a script for each item",
		Fields: []FieldSchema{
			{Key: "input_key", Label: "Input Key", Type: "text", DefaultValue: "items"},
			{Key: "body_script", Label: "Body Script", Type: "textarea", Required: true},
		},
		InputPorts:  []PortSchema{{Name: "items", Type: "array", Required: true}, {Name: "body_script", Type: "string", Required: true}},
		OutputPorts: []PortSchema{{Name: "iteration_results", Type: "array", Required: true}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "${input_key}", Type: "array", Desc: "The array to iterate over (default key: items)"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "iteration_results", Type: "array", Desc: "Collected results from body_script executions (only items where __iteration_result was set)"},
				{Key: "__iteration_item", Type: "any", Desc: "Current item during iteration (cleaned up after completion)"},
				{Key: "__iteration_index", Type: "int", Desc: "Current index during iteration (cleaned up after completion)"},
			},
			Notes: []string{
				"body_script runs on the SAME board (shared state) — it can read/write any board variable, which may cause side effects",
				"The body script must write to __iteration_result for its output to be collected; otherwise that iteration is silently skipped",
				"Temporary vars (__iteration_item, __iteration_index, __iteration_result) are cleaned up after iteration completes",
			},
		},
	},
	{
		Type: "script", Label: "Custom Script", Icon: "Code", Color: "gray", Category: "custom",
		Description: "Custom JavaScript script node",
		Fields: []FieldSchema{
			{Key: "source", Label: "Script Source", Type: "textarea", Required: true},
		},
		InputPorts:  []PortSchema{{Name: "input", Type: "any"}},
		OutputPorts: []PortSchema{{Name: "output", Type: "any"}},
		Runtime: &RuntimeSpec{
			BoardReads: []BoardVarSpec{
				{Key: "(any)", Type: "any", Desc: "Script can read any board variable via board.getVar(key)"},
			},
			BoardWrites: []BoardVarSpec{
				{Key: "(any)", Type: "any", Desc: "Script can write any board variable via board.setVar(key, value)"},
			},
			Notes: []string{
				"Full access to board, config, expr, stream, signal, shell, fs, runtime bridges",
				"Available bridges: board.getVar/setVar/getVars/hasVar, config (object), expr.eval, stream.emit, signal.done/interrupt, shell.exec, fs.read/write/exists/delete, runtime.execScript",
				"Custom scripts should document their board reads/writes in comments for maintainability",
			},
		},
	},
}
