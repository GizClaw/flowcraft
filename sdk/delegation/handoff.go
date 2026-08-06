package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/sdk/agent"
	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/tool"
)

const (
	// ToolName is the single LLM-facing function used for delegation.
	ToolName = "delegate"
	// HandoffStateKey is the agent.Result.State key containing HandoffEvent.
	HandoffStateKey = "handoff"
	// HandoffFinalizeReason prefixes the target in agent.Decision.Reason.
	HandoffFinalizeReason = "handoff:"
)

// Handoff declares a target available to the unified handoff tool.
type Handoff struct {
	Target Target
	// Filter decides whether Target is available for req. HandoffTool and
	// HandoffReferee always call it when non-nil, including when req is nil.
	Filter func(ctx context.Context, req *agent.Request) bool
	// OnInvoke is a lightweight synchronous observation hook. Dispatch remains
	// the host's responsibility after the referee records HandoffEvent.
	OnInvoke func(ctx context.Context, args HandoffArgs) error
}

// HandoffArgs is the JSON shape accepted by the unified handoff tool.
type HandoffArgs struct {
	Mode     Mode              `json:"mode"`
	Target   string            `json:"target"`
	Input    string            `json:"input"`
	Note     string            `json:"note,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func (a HandoffArgs) request() Request {
	return Request{
		Mode:     a.Mode,
		Target:   a.Target,
		Input:    a.Input,
		Metadata: a.Metadata,
	}
}

// HandoffEvent records the first matching handoff call in agent.Result.State.
type HandoffEvent struct {
	Target     string      `json:"target"`
	ToolCallID string      `json:"tool_call_id"`
	Args       HandoffArgs `json:"args"`
}

// HandoffTool builds the one "delegate" tool for the targets visible to req.
// Invalid or duplicate target declarations panic at assembly time.
func HandoffTool(ctx context.Context, req *agent.Request, handoffs []Handoff) tool.Tool {
	eligible := eligibleHandoffs(ctx, req, handoffs)
	if len(eligible) == 0 {
		panic("delegation.HandoffTool: at least one eligible target is required")
	}

	targetIDs := make([]string, 0, len(eligible))
	var descriptions strings.Builder
	descriptions.WriteString("Hand off the current interaction to one target: ")
	for i, handoff := range eligible {
		targetIDs = append(targetIDs, handoff.Target.ID)
		if i > 0 {
			descriptions.WriteString("; ")
		}
		descriptions.WriteString(handoff.Target.ID)
		if handoff.Target.Description != "" {
			descriptions.WriteString(" (")
			descriptions.WriteString(handoff.Target.Description)
			descriptions.WriteString(")")
		}
	}

	definition := message.DefineSchema(
		ToolName,
		descriptions.String(),
		message.ToolEnumProperty("mode", "string", "Delegation mode.", string(ModeHandoff)),
		message.ToolEnumProperty("target", "string", "Receiving target.", targetIDs...),
		message.ToolProperty("input", "string", "The task or user intent for the receiving target."),
		message.ToolProperty("note", "string", "Optional context for the receiving target."),
		message.ToolStringMapProperty("metadata", "Optional string metadata."),
	).Required("mode", "target", "input").DisallowAdditionalProperties().Build()

	byTarget := handoffMap(eligible)
	return tool.FuncTool(definition, func(ctx context.Context, raw string) (string, error) {
		var args HandoffArgs
		if err := decodeHandoffArgs([]byte(raw), &args); err != nil {
			return "", errdefs.Validationf("delegation: invalid handoff arguments: %v", err)
		}
		if err := args.request().Validate(); err != nil {
			return "", err
		}
		if args.Mode != ModeHandoff {
			return "", errdefs.Validationf("delegation: handoff tool requires mode %q", ModeHandoff)
		}
		handoff, ok := byTarget[args.Target]
		if !ok {
			return "", TargetNotFound(args.Target)
		}
		if handoff.OnInvoke != nil {
			if err := handoff.OnInvoke(ctx, args); err != nil {
				return "", err
			}
		}
		return "Handoff initiated to " + args.Target, nil
	})
}

// HandoffReferee returns an agent.Referee that recognizes calls to ToolName
// whose arguments strictly decode to a valid handoff request, whose target is
// eligible for the actual request, and whose matching tool result succeeded.
// Calls are scanned chronologically; the first matching call wins.
func HandoffReferee(handoffs []Handoff) agent.Referee {
	if len(handoffs) == 0 {
		return agent.BaseReferee{}
	}
	validateHandoffs(handoffs)
	return &handoffReferee{targets: handoffMap(handoffs)}
}

// DirectoryHandoffReferee returns an agent.Referee that validates handoff
// targets through directory at decision time. This lets a deployment capture
// an unbound directory during assembly and bind it before agent execution.
func DirectoryHandoffReferee(directory Directory) agent.Referee {
	if directory == nil {
		return agent.BaseReferee{}
	}
	return &handoffReferee{directory: directory}
}

type handoffReferee struct {
	agent.BaseReferee
	targets   map[string]Handoff
	directory Directory
}

func (r *handoffReferee) After(
	ctx context.Context,
	_ agent.Identity,
	req *agent.Request,
	result *agent.Result,
) (agent.Decision, error) {
	if result == nil {
		return agent.Decision{}, nil
	}
	successfulResults := make(map[string]struct{})
	for _, message := range result.Messages {
		for _, toolResult := range message.ToolResults() {
			if !toolResult.IsError {
				successfulResults[toolResult.CallID] = struct{}{}
			}
		}
	}
	for _, message := range result.Messages {
		for _, call := range message.ToolCalls() {
			if call.Name != ToolName {
				continue
			}
			if _, ok := successfulResults[call.ID]; !ok {
				continue
			}
			var args HandoffArgs
			if err := decodeHandoffArgs(call.Arguments, &args); err != nil ||
				args.request().Validate() != nil {
				continue
			}
			if args.Mode != ModeHandoff {
				continue
			}
			ok, err := r.acceptsTarget(ctx, req, args.Target)
			if err != nil {
				return agent.Decision{}, err
			}
			if !ok {
				continue
			}
			event := HandoffEvent{
				Target:     args.Target,
				ToolCallID: call.ID,
				Args:       args,
			}
			return agent.Decision{
				Reason: HandoffFinalizeReason + args.Target,
				State:  map[string]any{HandoffStateKey: event},
			}, nil
		}
	}
	return agent.Decision{}, nil
}

func (r *handoffReferee) acceptsTarget(ctx context.Context, req *agent.Request, id string) (bool, error) {
	if r.directory == nil {
		handoff, ok := r.targets[id]
		if !ok || !supportsMode(handoff.Target, ModeHandoff) {
			return false, nil
		}
		return handoff.Filter == nil || handoff.Filter(ctx, req), nil
	}
	target, err := r.directory.Get(ctx, id)
	if err != nil {
		if errdefs.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if err := target.Validate(); err != nil {
		return false, err
	}
	return supportsMode(target, ModeHandoff), nil
}

func decodeHandoffArgs(raw []byte, args *HandoffArgs) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(args); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON")
	}
	return nil
}

// HandoffFromResult extracts the structured event recorded by HandoffReferee.
func HandoffFromResult(result *agent.Result) (HandoffEvent, bool) {
	if result == nil || result.State == nil {
		return HandoffEvent{}, false
	}
	event, ok := result.State[HandoffStateKey].(HandoffEvent)
	return event, ok
}

func eligibleHandoffs(ctx context.Context, req *agent.Request, handoffs []Handoff) []Handoff {
	validateHandoffs(handoffs)
	result := make([]Handoff, 0, len(handoffs))
	for _, handoff := range handoffs {
		if !supportsMode(handoff.Target, ModeHandoff) {
			continue
		}
		if handoff.Filter != nil && !handoff.Filter(ctx, req) {
			continue
		}
		result = append(result, handoff)
	}
	return result
}

func validateHandoffs(handoffs []Handoff) {
	seen := make(map[string]struct{}, len(handoffs))
	for _, handoff := range handoffs {
		if err := handoff.Target.Validate(); err != nil {
			panic(fmt.Sprintf("delegation: invalid handoff target: %v", err))
		}
		if _, ok := seen[handoff.Target.ID]; ok {
			panic(fmt.Sprintf("delegation: duplicate handoff target %q", handoff.Target.ID))
		}
		seen[handoff.Target.ID] = struct{}{}
	}
}

func supportsMode(target Target, mode Mode) bool {
	// An empty mode list means the target is unrestricted and supports every
	// defined mode. A non-empty list is an explicit allowlist.
	if len(target.Modes) == 0 {
		return true
	}
	for _, supported := range target.Modes {
		if supported == mode {
			return true
		}
	}
	return false
}

func handoffMap(handoffs []Handoff) map[string]Handoff {
	result := make(map[string]Handoff, len(handoffs))
	for _, handoff := range handoffs {
		result[handoff.Target.ID] = handoff
	}
	return result
}

var _ agent.Referee = (*handoffReferee)(nil)
