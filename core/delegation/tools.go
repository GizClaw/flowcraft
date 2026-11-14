package delegation

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
)

// Tools returns the model-facing delegation tools.
func Tools(service *Service) tool.Source {
	return toolSource{service: service}
}

type toolSource struct{ service *Service }

func (s toolSource) Tools() []tool.Tool {
	if s.service == nil {
		return nil
	}
	return []tool.Tool{
		newDelegate(s.service),
		newTargets(s.service),
		newStatus(s.service),
		newAnswer(s.service),
		newCancel(s.service),
	}
}

func (toolSource) LazyTools() []tool.LazyTool { return nil }

func newDelegate(service *Service) tool.Tool {
	definition := message.ToolDefinition{
		Name:        "delegate",
		Description: "Delegate work to another deployed agent and return its answer. The target runs under its own deployment policy; this tool cannot change it.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"mode": {"type": "string", "enum": ["sync", "async"]},
				"target": {"type": "string"},
				"input": {"type": "string"}
			},
			"required": ["target", "input"]
		}`),
	}
	return tool.FuncTool(definition, func(ctx context.Context, arguments string) (message.Content, error) {
		var args struct {
			Mode   string `json:"mode"`
			Target string `json:"target"`
			Input  string `json:"input"`
		}
		decoder := json.NewDecoder(strings.NewReader(arguments))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&args); err != nil {
			return message.Content{}, errdefs.Validationf("delegate: parse arguments: %w", err)
		}
		mode := Mode(args.Mode)
		if mode == "" {
			mode = ModeSync
		}
		outcome, err := service.Delegate(ctx, Request{
			Mode:   mode,
			Target: args.Target,
			Input:  args.Input,
		})
		if err != nil {
			return message.Content{}, err
		}
		return outcomeContent(outcome)
	})
}

func newTargets(service *Service) tool.Tool {
	definition := message.ToolDefinition{
		Name:        "delegation_targets",
		Description: "List the deployed agents this agent may delegate to.",
		InputSchema: json.RawMessage(`{"type": "object", "properties": {}}`),
	}
	return tool.FuncTool(definition, func(ctx context.Context, arguments string) (message.Content, error) {
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		var args struct{}
		decoder := json.NewDecoder(strings.NewReader(arguments))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&args); err != nil {
			return message.Content{}, errdefs.Validationf("delegation_targets: parse arguments: %w", err)
		}
		targets, err := service.Targets(ctx)
		if err != nil {
			return message.Content{}, err
		}
		raw, err := json.Marshal(map[string]any{"targets": targets})
		if err != nil {
			return message.Content{}, errdefs.Internalf("delegation_targets: encode: %w", err)
		}
		return message.NewJSONContent(raw)
	})
}

func outcomeContent(outcome Outcome) (message.Content, error) {
	if outcome.Status == StatusCompleted && len(outcome.Content.Parts) > 0 {
		return outcome.Content, nil
	}
	value := map[string]any{
		"status":        string(outcome.Status),
		"delegation_id": outcome.ID,
		"target":        outcome.Target,
		"child_session": outcome.ChildSession,
		"depth":         outcome.Depth,
		"chain_id":      outcome.ChainID,
	}
	if outcome.Error != "" {
		value["error"] = outcome.Error
	}
	if outcome.Question != nil {
		value["question"] = outcome.Question.Text()
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return message.Content{}, errdefs.Internalf("delegate: encode outcome: %w", err)
	}
	content, err := message.NewJSONContent(raw)
	if err != nil {
		return message.Content{}, fmt.Errorf("delegate: %w", err)
	}
	return content, nil
}

func newStatus(service *Service) tool.Tool {
	definition := message.ToolDefinition{
		Name:        "delegation_status",
		Description: "Read one asynchronous delegation by its delegation_id.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"delegation_id": {"type": "string"}},
			"required": ["delegation_id"]
		}`),
	}
	return tool.FuncTool(definition, func(ctx context.Context, arguments string) (message.Content, error) {
		id, err := delegationID(arguments, "delegation_status")
		if err != nil {
			return message.Content{}, err
		}
		outcome, err := service.Status(ctx, id)
		if err != nil {
			return message.Content{}, err
		}
		return outcomeContent(outcome)
	})
}

func newAnswer(service *Service) tool.Tool {
	definition := message.ToolDefinition{
		Name:        "delegation_answer",
		Description: "Answer a delegation that is waiting for input.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"delegation_id": {"type": "string"},
				"answer": {"type": "string"}
			},
			"required": ["delegation_id", "answer"]
		}`),
	}
	return tool.FuncTool(definition, func(ctx context.Context, arguments string) (message.Content, error) {
		var args struct {
			ID     string `json:"delegation_id"`
			Answer string `json:"answer"`
		}
		if err := decodeArgs(arguments, &args); err != nil {
			return message.Content{}, errdefs.Validationf("delegation_answer: %w", err)
		}
		outcome, err := service.Answer(ctx, args.ID, args.Answer)
		if err != nil {
			return message.Content{}, err
		}
		return outcomeContent(outcome)
	})
}

func newCancel(service *Service) tool.Tool {
	definition := message.ToolDefinition{
		Name:        "delegation_cancel",
		Description: "Cancel one asynchronous delegation.",
		InputSchema: json.RawMessage(`{
			"type": "object",
			"properties": {"delegation_id": {"type": "string"}},
			"required": ["delegation_id"]
		}`),
	}
	return tool.FuncTool(definition, func(ctx context.Context, arguments string) (message.Content, error) {
		id, err := delegationID(arguments, "delegation_cancel")
		if err != nil {
			return message.Content{}, err
		}
		outcome, err := service.Cancel(ctx, id)
		if err != nil {
			return message.Content{}, err
		}
		return outcomeContent(outcome)
	})
}

func delegationID(arguments, toolName string) (string, error) {
	var args struct {
		ID string `json:"delegation_id"`
	}
	if err := decodeArgs(arguments, &args); err != nil {
		return "", errdefs.Validationf("%s: %w", toolName, err)
	}
	if strings.TrimSpace(args.ID) == "" {
		return "", errdefs.Validationf("%s: delegation_id is required", toolName)
	}
	return args.ID, nil
}

func decodeArgs(arguments string, into any) error {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	decoder := json.NewDecoder(strings.NewReader(arguments))
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}
