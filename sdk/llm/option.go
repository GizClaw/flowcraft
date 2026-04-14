package llm

// GenerateOption configures a single Generate/GenerateStream call.
type GenerateOption func(*GenerateOptions)

// JSONSchemaParam describes a JSON Schema for structured output.
type JSONSchemaParam struct {
	Name        string
	Description string
	Schema      any
	Strict      bool
}

// GenerateOptions holds all generation parameters.
type GenerateOptions struct {
	Temperature      *float64
	MaxTokens        *int64
	TopP             *float64
	TopK             *int64
	StopWords        []string
	FrequencyPenalty *float64
	PresencePenalty  *float64
	JSONMode         *bool
	JSONSchema       *JSONSchemaParam
	Tools            []ToolDefinition
	ToolChoice       *ToolChoice

	// Thinking controls the model's reasoning/thinking mode.
	// Providers that support it map this to their own API format;
	// unsupported providers simply ignore the field.
	Thinking *bool

	// Extra carries provider-specific parameters as key-value pairs.
	// Providers read the keys they care about and ignore the rest.
	Extra map[string]any
}

// ToolChoiceType specifies how the model selects tools.
type ToolChoiceType string

const (
	ToolChoiceAuto     ToolChoiceType = "auto"
	ToolChoiceNone     ToolChoiceType = "none"
	ToolChoiceRequired ToolChoiceType = "required"
	ToolChoiceSpecific ToolChoiceType = "specific"
)

// ToolChoice controls whether and which tools the model should use.
type ToolChoice struct {
	Type ToolChoiceType `json:"type"`
	Name string         `json:"name,omitempty"`
}

// ApplyOptions folds option funcs into a GenerateOptions value.
func ApplyOptions(opts ...GenerateOption) *GenerateOptions {
	o := &GenerateOptions{}
	for _, fn := range opts {
		fn(o)
	}
	return o
}

func WithTemperature(t float64) GenerateOption {
	return func(o *GenerateOptions) { o.Temperature = &t }
}

func WithMaxTokens(n int64) GenerateOption {
	return func(o *GenerateOptions) { o.MaxTokens = &n }
}

func WithTopP(p float64) GenerateOption {
	return func(o *GenerateOptions) { o.TopP = &p }
}

func WithTopK(k int64) GenerateOption {
	return func(o *GenerateOptions) { o.TopK = &k }
}

func WithStopWords(words ...string) GenerateOption {
	return func(o *GenerateOptions) { o.StopWords = words }
}

func WithFrequencyPenalty(p float64) GenerateOption {
	return func(o *GenerateOptions) { o.FrequencyPenalty = &p }
}

func WithPresencePenalty(p float64) GenerateOption {
	return func(o *GenerateOptions) { o.PresencePenalty = &p }
}

func WithJSONMode(on bool) GenerateOption {
	return func(o *GenerateOptions) { o.JSONMode = &on }
}

func WithJSONSchema(schema JSONSchemaParam) GenerateOption {
	return func(o *GenerateOptions) { o.JSONSchema = &schema }
}

func WithTools(tools ...ToolDefinition) GenerateOption {
	return func(o *GenerateOptions) { o.Tools = tools }
}

func WithToolChoice(choice ToolChoice) GenerateOption {
	return func(o *GenerateOptions) { o.ToolChoice = &choice }
}

func WithToolChoiceAuto() GenerateOption {
	return WithToolChoice(ToolChoice{Type: ToolChoiceAuto})
}

func WithToolChoiceNone() GenerateOption {
	return WithToolChoice(ToolChoice{Type: ToolChoiceNone})
}

func WithToolChoiceRequired() GenerateOption {
	return WithToolChoice(ToolChoice{Type: ToolChoiceRequired})
}

func WithToolChoiceSpecific(name string) GenerateOption {
	return WithToolChoice(ToolChoice{Type: ToolChoiceSpecific, Name: name})
}

func WithThinking(on bool) GenerateOption {
	return func(o *GenerateOptions) { o.Thinking = &on }
}

func WithExtra(key string, value any) GenerateOption {
	return func(o *GenerateOptions) {
		if o.Extra == nil {
			o.Extra = make(map[string]any)
		}
		o.Extra[key] = value
	}
}
