// Package ask implements the built-in ask_user tool: one question to the conversation's user, and
// the answer as the call's result.
//
// The tool does not block. Its pre-flight records the question, the turn suspends on it with
// [tool.Asker], and the answer arrives as the session's resume; Execute then returns it. A blocking
// ask would hold the turn — and, for a delegated child, the parent's turn with it — for as long as a
// person takes to answer.
package ask

import (
	"context"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/goccy/go-json"
)

// Name is the tool the model calls.
const Name = "ask_user"

// Tool is the ask_user tool. It holds nothing: the conversation's question route arrives with each
// call, because one tool serves every session of a deployment.
type Tool struct{}

// New builds the tool.
func New() *Tool { return &Tool{} }

// Tools returns the one tool of this source.
func (*Tool) Tools() []tool.Tool { return []tool.Tool{askTool{}} }

// LazyTools is empty: the schema is static and cheap.
func (*Tool) LazyTools() []tool.LazyTool { return nil }

type askTool struct{}

var _ tool.Tool = askTool{}

// Needs declares the question surface: a deployment that does not provide it refuses this tool at
// registration instead of failing on the first call.
func (askTool) Needs() []tool.Need { return []tool.Need{platform.AskSlot} }

func (askTool) Definition() message.ToolDefinition {
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"question": map[string]any{
				"type":        "string",
				"description": "What to ask. The conversation pauses until it is answered.",
			},
		},
		"required":             []string{"question"},
		"additionalProperties": false,
	})
	if err != nil {
		panic("ask_user: schema: " + err.Error())
	}
	return message.ToolDefinition{
		Name: Name,
		Description: "Ask the user a question and receive their answer as this call's result. " +
			"Use it when the answer cannot be derived and the work cannot continue without it; " +
			"the turn stops until they reply.",
		InputSchema: schema,
	}
}

// Metadata marks the tool as not mutating: it changes the conversation, not the world.
func (askTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

// Ask is the pre-flight: it records the question and returns the text to suspend on, or nil when the
// session already holds the answer.
func (askTool) Ask(ctx context.Context, binding tool.Binding, arguments string) (*message.Content, error) {
	questions, err := surface(binding)
	if err != nil {
		return nil, err
	}
	question, err := decode(arguments)
	if err != nil {
		return nil, err
	}
	return questions.Ask(ctx, question), nil
}

// Execute returns the answer the session is holding.
func (askTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	questions, err := surface(binding)
	if err != nil {
		return message.Content{}, err
	}
	if _, err := decode(arguments); err != nil {
		return message.Content{}, err
	}
	answer, ok := questions.Take(ctx)
	if !ok {
		// The pre-flight guarantees this: a call only runs on a turn whose Ask returned nil, and
		// Ask returns nil only when the session holds an answer.
		return message.Content{}, errdefs.Internalf("%s: no answer is waiting", Name)
	}
	return answer, nil
}

// surface reads the question slot, or explains that this deployment has no outlet for questions.
func surface(binding tool.Binding) (platform.Questions, error) {
	questions, ok := platform.AskSlot.Get(binding.Caps)
	if !ok || questions == nil {
		return nil, errdefs.NotAvailablef(
			"%s: this deployment has no outlet for questions (install host.WithQuestions)", Name)
	}
	return questions, nil
}

// decode reads the one argument.
func decode(arguments string) (message.Content, error) {
	var params struct {
		Question string `json:"question"`
	}
	if err := tool.DecodeArgs(Name, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	if params.Question == "" {
		return message.Content{}, errdefs.Validationf("%s: question is required", Name)
	}
	return message.NewTextContent(params.Question), nil
}
