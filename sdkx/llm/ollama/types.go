package ollama

type chatMessage struct {
	Role      string           `json:"role"`
	Content   string           `json:"content,omitempty"`
	Images    []string         `json:"images,omitempty"`
	ToolCalls []ollamaToolCall `json:"tool_calls,omitempty"`
}

type ollamaToolCall struct {
	Function ollamaFunctionCall `json:"function"`
}

type ollamaFunctionCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ollamaTool struct {
	Type     string             `json:"type"`
	Function ollamaToolFunction `json:"function"`
}

type ollamaToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Stream   bool          `json:"stream"`
	Think    *bool         `json:"think,omitempty"`
	Format   any           `json:"format,omitempty"`
	Options  *chatOptions  `json:"options,omitempty"`
	Tools    []ollamaTool  `json:"tools,omitempty"`
}

type chatOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	TopP        *float64 `json:"top_p,omitempty"`
	TopK        *int64   `json:"top_k,omitempty"`
	NumPredict  *int64   `json:"num_predict,omitempty"`
	Stop        []string `json:"stop,omitempty"`
	Frequency   *float64 `json:"frequency_penalty,omitempty"`
	Presence    *float64 `json:"presence_penalty,omitempty"`
}

type chatResponse struct {
	Message    chatMessage `json:"message"`
	Done       bool        `json:"done"`
	DoneReason string      `json:"done_reason,omitempty"`

	PromptEvalCount int64 `json:"prompt_eval_count,omitempty"`
	EvalCount       int64 `json:"eval_count,omitempty"`

	TotalDuration      int64 `json:"total_duration,omitempty"`
	LoadDuration       int64 `json:"load_duration,omitempty"`
	PromptEvalDuration int64 `json:"prompt_eval_duration,omitempty"`
	EvalDuration       int64 `json:"eval_duration,omitempty"`
}
