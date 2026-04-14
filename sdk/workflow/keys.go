package workflow

// Board variable keys for the workflow/runtime layer.
const (
	VarQuery            = "query"
	VarAnswer           = "answer"
	VarMessages         = "messages"
	VarRunID            = "__run_id"
	VarStartTime        = "__start_time"
	VarInternalUsage    = "__usage"
	VarInterruptedNode  = "__interrupted_node"
	VarOutputSchema     = "__output_schema"
	VarPrevMessageCount = "__prev_message_count"
	VarSummaryIndex     = "__summary_index"
)
