package node

// Board variable keys used by builtin node implementations (LLM, template, etc.).
const (
	VarResponse    = "response"
	VarUsage       = "usage"
	VarToolPending = "tool_pending"
	VarToolOutput  = "tool_output"
	VarToolCalls   = "__tool_calls"
)
