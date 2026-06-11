package engine

import (
	"context"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/event"
)

// Stream-delta emission helpers
// -----------------------------
//
// SubjectStreamDelta + StreamDeltaPayload are the SDK-wide SPI that
// in-flight increments — assistant tokens, tool calls, tool results —
// flow through. Anyone with a [Publisher] can emit them; the contract is
// not LLM-specific. These helpers package the boilerplate (envelope
// construction, well-known headers, payload validation) so a custom
// node, a wrapper engine, or a test harness can publish a valid stream
// delta in a single line:
//
//	// Inside a custom long-running graph node:
//	engine.EmitStreamToken(ctx, pub, runID, nodeID, "loaded chunk 3/10")
//
// They are sugar over [SubjectStreamDelta] + [event.NewEnvelope] —
// callers that need fine-grained control (custom headers, batched
// publish) can still construct the envelope by hand. The helpers do
// nothing if pub is nil so a node that lost its Publisher (e.g. a
// host built with NoopHost{}) keeps running.

// EmitStreamToken publishes one assistant-token delta on the canonical
// stream subject. See [EmitStreamDelta] for the stepActor format
// requirement.
//
// Use this from any node that produces incremental textual output —
// for example a custom RAG retriever streaming its working notes, or a
// post-processing node turning structured data into prose. content may
// be empty (callers that want "still alive" heartbeats should typically
// mark them differently); empty content is published as-is so the
// helper stays predictable.
func EmitStreamToken(ctx context.Context, pub Publisher, runID, stepActor, content string) error {
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type:    StreamDeltaToken,
		Content: content,
	})
}

// EmitStreamToolCall publishes one tool-call delta. id and name are
// required (consumers correlate the eventual tool_result by ID); args
// is the tool input the model produced and may be either a JSON string
// or an already-decoded map / slice — both are valid per the
// [StreamDeltaPayload] contract. See [EmitStreamDelta] for the
// stepActor format requirement.
//
// The helper validates the required fields up-front and returns a
// descriptive error instead of publishing a malformed envelope; callers
// that already validated upstream can ignore the error safely.
func EmitStreamToolCall(ctx context.Context, pub Publisher, runID, stepActor, id, name string, args any) error {
	if id == "" {
		return fmt.Errorf("engine: EmitStreamToolCall: id is required")
	}
	if name == "" {
		return fmt.Errorf("engine: EmitStreamToolCall: name is required")
	}
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type:      StreamDeltaToolCall,
		ID:        id,
		Name:      name,
		Arguments: args,
	})
}

// EmitStreamToolResult publishes one tool-result delta. toolCallID and
// content are required (toolCallID pairs the result with the originating
// tool_call); name is recommended so consumers can render the result
// without a separate dispatch table. isError marks unsuccessful
// results; cancelled marks synthesised cancellations emitted when the
// round was interrupted before the call dispatched. See
// [EmitStreamDelta] for the stepActor format requirement.
func EmitStreamToolResult(ctx context.Context, pub Publisher, runID, stepActor, toolCallID, name, content string, isError, cancelled bool) error {
	if toolCallID == "" {
		return fmt.Errorf("engine: EmitStreamToolResult: toolCallID is required")
	}
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type:       StreamDeltaToolResult,
		ToolCallID: toolCallID,
		Name:       name,
		Content:    content,
		IsError:    isError,
		Cancelled:  cancelled,
	})
}

// EmitStreamDelta is the low-level form of the EmitStreamX helpers.
// Custom nodes that need to set fields outside the type-specific
// helpers (e.g. a forward-compatible Type the SDK does not yet ship a
// helper for) build the payload themselves and pass it here. Required
// per-Type fields are validated to mirror the contract enforced by
// [DecodeStreamDelta] on the consumer side, so a malformed delta is
// caught at publish time instead of silently flowing to subscribers.
//
// stepActor follows the contract documented at the top of subjects.go:
// it MUST start with the executing agent.id (so [PatternRunAgentStream]
// can fan-in by agent) and MAY append an engine-private suffix
// (graph runner: ".node.<nodeID>"; embedded loop engine: ".iter<N>"). Both
// runID and stepActor are sanitised by [SanitiseID] so caller-supplied
// values cannot fragment the resulting subject.
//
// The envelope is stamped with HeaderRunID. The agent identifier is
// derived from the stepActor segment ahead of any optional ".node." /
// ".iter" suffix — it goes onto HeaderAgentID (and the legacy
// HeaderActorID via [event.Envelope.SetAgentID] dual-write). For
// header-routed subscribers that key off the node id, the
// HeaderNodeID is populated whenever stepActor carries the
// graph runner's "<agent>.node.<nodeID>" form so the two transports
// stay aligned.
//
// Publish errors are returned to the caller (unlike the executor's
// fire-and-forget convention) so node authors can decide whether to
// retry or surface the failure; in practice most callers just discard
// the error because stream deltas are observability, not control flow.
func EmitStreamDelta(ctx context.Context, pub Publisher, runID, stepActor string, payload StreamDeltaPayload) error {
	if pub == nil {
		return nil
	}
	if err := validateStreamDelta(payload); err != nil {
		return err
	}
	subject := SubjectStreamDelta(runID, stepActor)
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return err
	}
	if runID != "" {
		env.SetRunID(runID)
	}
	agentID, nodeID := splitStepActor(stepActor)
	if agentID != "" {
		env.SetAgentID(agentID)
	}
	if nodeID != "" {
		env.SetNodeID(nodeID)
	}
	return pub.Publish(ctx, env)
}

// splitStepActor extracts the agent.id prefix and the optional graph
// runner ".node.<nodeID>" suffix from a stepActor string. Returns
// (stepActor, "") when no recognised suffix is present, so engines
// that use a different suffix scheme (e.g. an embedded loop engine's ".iter<N>")
// only get the agent.id projected onto HeaderAgentID and rely on
// other facilities for the rest.
//
// Kept private because the suffix vocabulary is not part of any
// consumer-facing contract — only the agent.id prefix is.
func splitStepActor(stepActor string) (agentID, nodeID string) {
	const nodeSep = ".node."
	for i := 0; i+len(nodeSep) <= len(stepActor); i++ {
		if stepActor[i:i+len(nodeSep)] == nodeSep {
			return stepActor[:i], stepActor[i+len(nodeSep):]
		}
	}
	return stepActor, ""
}

// validateStreamDelta mirrors the per-Type field requirements
// documented on [StreamDeltaPayload]. We deliberately do NOT validate
// unknown Type values: the contract says consumers SHOULD treat
// unknowns as forward-compatible, so the helper does the same on the
// emit side.
func validateStreamDelta(p StreamDeltaPayload) error {
	switch p.Type {
	case StreamDeltaToken:
		// Content is allowed to be empty — see EmitStreamToken docs.
		return nil
	case StreamDeltaToolCall:
		if p.ID == "" {
			return fmt.Errorf("engine: stream delta tool_call requires ID")
		}
		if p.Name == "" {
			return fmt.Errorf("engine: stream delta tool_call requires Name")
		}
		return nil
	case StreamDeltaToolResult:
		if p.ToolCallID == "" {
			return fmt.Errorf("engine: stream delta tool_result requires ToolCallID")
		}
		return nil
	case StreamDeltaParallelBranchAccept, StreamDeltaParallelBranchCancel:
		if p.ForkID == "" {
			return fmt.Errorf("engine: stream delta %s requires ForkID", p.Type)
		}
		if p.BranchID == "" {
			return fmt.Errorf("engine: stream delta %s requires BranchID", p.Type)
		}
		return nil
	case "":
		return fmt.Errorf("engine: stream delta requires Type")
	default:
		// Forward-compatible Type — accept it.
		return nil
	}
}
