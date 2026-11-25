package delegation

import (
	"context"
	"fmt"
	"strings"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/platform"
	"github.com/goccy/go-json"
)

// Name is the tool the model calls.
const (
	Name   = "delegate"
	Status = "delegation_status"
	Answer = "delegation_answer"
	Cancel = "delegation_cancel"
)

// Modes the tool accepts, as the document's policy spells them.
const (
	ModeSync  = "sync"
	ModeAsync = "async"
)

// Tool is the delegate tool. It holds nothing: the capability arrives with each call, because a
// delegation is admitted against the calling session's policy and a tool is shared by every session.
type Tool struct{}

// New builds the delegate tool.
func New() *Tool { return &Tool{} }

// Tools returns the one tool of this source.
func (*Tool) Tools() []tool.Tool {
	return []tool.Tool{delegateTool{}, statusTool{}, answerTool{}, cancelTool{}}
}

// LazyTools is empty: the schema is static and cheap.
func (*Tool) LazyTools() []tool.LazyTool { return nil }

type delegateTool struct{}

var _ tool.Tool = delegateTool{}

// Needs declares the capability: a session whose grant names no delegation policy is not given the
// slot, so this tool is refused at registration rather than failing on its first call.
func (delegateTool) Needs() []tool.Need { return []tool.Need{platform.DelegationSlot} }

func (delegateTool) Definition() message.ToolDefinition {
	schema, err := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"target": map[string]any{
				"type":        "string",
				"description": "The agent to run.",
			},
			"input": map[string]any{
				"type":        "string",
				"description": "The message the other agent starts from.",
			},
			"mode": map[string]any{
				"type":        "string",
				"enum":        []string{ModeSync, ModeAsync},
				"default":     ModeSync,
				"description": "sync waits for the answer; async hands the work off and returns the session.",
			},
		},
		"required":             []string{"target", "input"},
		"additionalProperties": false,
	})
	if err != nil {
		panic("delegation: schema: " + err.Error())
	}
	return message.ToolDefinition{
		Name: Name,
		Description: "Run another agent and return what it produced, or hand the work off and return " +
			"its session. Use sync when the answer is needed to continue; use async when the work can " +
			"proceed without it — " + Status + ", " + Answer + " and " + Cancel + " then address the " +
			"session it returned. Which agents are reachable, and which of the two modes, is the " +
			"deployment's policy: a refusal names what the policy allows.",
		InputSchema: schema,
	}
}

func (delegateTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (delegateTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	var params struct {
		Target string `json:"target"`
		Input  string `json:"input"`
		Mode   string `json:"mode"`
	}
	if err := tool.DecodeArgs(Name, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	if strings.TrimSpace(params.Target) == "" {
		return message.Content{}, errdefs.Validationf("%s: target is required", Name)
	}
	mode := strings.TrimSpace(params.Mode)
	if mode == "" {
		mode = ModeSync
	}
	delegate, ok := platform.DelegationSlot.Get(binding.Caps)
	if !ok || delegate == nil {
		return message.Content{}, errdefs.NotAvailablef(
			"%s: this session may not delegate", Name)
	}

	outcome, err := delegate.Delegate(ctx, platform.DelegationRequest{
		Mode:   mode,
		Target: params.Target,
		Input:  params.Input,
	})
	if err != nil {
		return message.Content{}, err
	}
	// The hand-off is the one delegation nobody observes afterwards: the caller gets an address and the
	// turn that asked for it is over. A line here is what an operator has to correlate it with.
	if mode == ModeAsync {
		telemetry.Info(ctx, "delegation: handed off "+String(outcome),
			telemetry.String("delegation.target", params.Target),
			telemetry.Int("delegation.depth", outcome.Depth))
	}
	payload, err := json.Marshal(answer(outcome))
	if err != nil {
		return message.Content{}, errdefs.Internalf("%s: encode result: %v", Name, err)
	}
	return message.NewTextContent(string(payload)), nil
}

// answer is what the model reads back. It carries the child's answer when there is one, the question
// when it paused, and the address either way — an async hand-off has nothing else to report, and a
// sync answer that dropped the address would leave the caller unable to follow up.
func answer(outcome platform.DelegationOutcome) map[string]any {
	out := map[string]any{
		"session": outcome.Session,
		"status":  outcome.Status,
		"chain":   outcome.Chain,
		"depth":   outcome.Depth,
	}
	if text := outcome.Content.Text(); text != "" {
		out["content"] = text
	}
	if outcome.Question != nil {
		out["question"] = outcome.Question.Text()
	}
	return out
}

// String renders a delegation for a log line: the edge, its state and its depth, never the payload.
//
// It is what the async hand-off line is built from — the one place this package reports something an
// operator reads, because it is the one delegation no caller follows afterwards.
func String(outcome platform.DelegationOutcome) string {
	return fmt.Sprintf("%s (%s, depth %d)", outcome.Session, outcome.Status, outcome.Depth)
}

// tools beyond delegate address a child the caller already has the id of.
type statusTool struct{}

var _ tool.Tool = statusTool{}

func (statusTool) Needs() []tool.Need { return []tool.Need{platform.DelegationSlot} }

func (statusTool) Definition() message.ToolDefinition {
	return oneField(Status, "Report where a delegated agent stands.",
		"session", "The session id "+Name+" returned.")
}

func (statusTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

func (statusTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	var params struct {
		Session string `json:"session"`
	}
	if err := tool.DecodeArgs(Status, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	delegate, err := capability(binding, Status)
	if err != nil {
		return message.Content{}, err
	}
	outcome, err := delegate.Status(ctx, params.Session)
	if err != nil {
		return message.Content{}, err
	}
	return encode(Status, answer(outcome))
}

type answerTool struct{}

var _ tool.Tool = answerTool{}

func (answerTool) Needs() []tool.Need { return []tool.Need{platform.DelegationSlot} }

func (answerTool) Definition() message.ToolDefinition {
	return schemaOf(Answer,
		"Answer a delegated agent that paused for input. Give a decision for an approval "+
			"question, and text for anything else. A denial is final: the child cannot escalate "+
			"it afterwards, so escalate instead when you would rather not judge.",
		map[string]any{
			"session": map[string]any{
				"type":        "string",
				"description": "The session id " + Name + " returned.",
			},
			"decision": map[string]any{
				"type": "string",
				"enum": []string{platform.DecisionApprove, platform.DecisionDeny, platform.DecisionEscalate},
				"description": "The answer to an approval question: approve runs the command, deny " +
					"refuses it for good, and escalate asks the deployment's approver.",
			},
			"answer": map[string]any{
				"type": "string",
				"description": "The reply a child that asked something other than an approval " +
					"continues from, or the reason beside a decision.",
			},
		}, "session")
}

func (answerTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (answerTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	var params struct {
		Session  string `json:"session"`
		Decision string `json:"decision"`
		Answer   string `json:"answer"`
	}
	if err := tool.DecodeArgs(Answer, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	if params.Decision == "" && strings.TrimSpace(params.Answer) == "" {
		return message.Content{}, errdefs.Validationf(
			"%s: give a decision for an approval question, or the answer to any other", Answer)
	}
	delegate, err := capability(binding, Answer)
	if err != nil {
		return message.Content{}, err
	}
	outcome, err := delegate.Answer(ctx, params.Session, platform.DelegationAnswer{
		Decision: params.Decision,
		Text:     params.Answer,
	})
	if err != nil {
		return message.Content{}, err
	}
	return encode(Answer, answer(outcome))
}

type cancelTool struct{}

var _ tool.Tool = cancelTool{}

func (cancelTool) Needs() []tool.Need { return []tool.Need{platform.DelegationSlot} }

func (cancelTool) Definition() message.ToolDefinition {
	return oneField(Cancel, "Stop a delegated agent.", "session", "The session id "+Name+" returned.")
}

func (cancelTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (cancelTool) Execute(ctx context.Context, binding tool.Binding, arguments string) (message.Content, error) {
	var params struct {
		Session string `json:"session"`
	}
	if err := tool.DecodeArgs(Cancel, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	delegate, err := capability(binding, Cancel)
	if err != nil {
		return message.Content{}, err
	}
	if err := delegate.Cancel(ctx, params.Session, "canceled by the caller"); err != nil {
		return message.Content{}, err
	}
	return encode(Cancel, map[string]any{"session": params.Session, "status": "canceled"})
}

// capability reads the delegation slot, or says the session may not delegate.
func capability(binding tool.Binding, name string) (platform.Delegation, error) {
	delegate, ok := platform.DelegationSlot.Get(binding.Caps)
	if !ok || delegate == nil {
		return nil, errdefs.NotAvailablef("%s: this session may not delegate", name)
	}
	return delegate, nil
}

// encode renders one tool answer.
func encode(toolName string, payload any) (message.Content, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return message.Content{}, errdefs.Internalf("%s: encode result: %v", toolName, err)
	}
	return message.NewTextContent(string(raw)), nil
}

// oneField builds a closed schema, so a typo in an argument is a schema error rather than a silently
// ignored setting.
func oneField(name, description, field, fieldDescription string) message.ToolDefinition {
	return schemaOf(name, description, map[string]any{
		field: map[string]any{"type": "string", "description": fieldDescription},
	}, field)
}

func schemaOf(name, description string, properties map[string]any, required ...string) message.ToolDefinition {
	raw, err := json.Marshal(map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             required,
		"additionalProperties": false,
	})
	if err != nil {
		panic("delegation: schema: " + name + ": " + err.Error())
	}
	return message.ToolDefinition{Name: name, Description: description, InputSchema: raw}
}
