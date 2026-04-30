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
// stream subject. The payload is a [StreamDeltaPayload] of type
// [StreamDeltaToken] with Content set; runID and actorID feed the
// subject builder so any consumer subscribed via [PatternRunStream]
// observes it.
//
// Use this from any node that produces incremental textual output —
// for example a custom RAG retriever streaming its working notes, or a
// post-processing node turning structured data into prose. content may
// be empty (callers that want "still alive" heartbeats should typically
// mark them differently); empty content is published as-is so the
// helper stays predictable.
func EmitStreamToken(ctx context.Context, pub Publisher, runID, actorID, content string) error {
	return EmitStreamDelta(ctx, pub, runID, actorID, StreamDeltaPayload{
		Type:    StreamDeltaToken,
		Content: content,
	})
}

// EmitStreamToolCall publishes one tool-call delta. id and name are
// required (consumers correlate the eventual tool_result by ID); args
// is the tool input the model produced and may be either a JSON string
// or an already-decoded map / slice — both are valid per the
// [StreamDeltaPayload] contract.
//
// The helper validates the required fields up-front and returns a
// descriptive error instead of publishing a malformed envelope; callers
// that already validated upstream can ignore the error safely.
func EmitStreamToolCall(ctx context.Context, pub Publisher, runID, actorID, id, name string, args any) error {
	if id == "" {
		return fmt.Errorf("engine: EmitStreamToolCall: id is required")
	}
	if name == "" {
		return fmt.Errorf("engine: EmitStreamToolCall: name is required")
	}
	return EmitStreamDelta(ctx, pub, runID, actorID, StreamDeltaPayload{
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
// round was interrupted before the call dispatched.
func EmitStreamToolResult(ctx context.Context, pub Publisher, runID, actorID, toolCallID, name, content string, isError, cancelled bool) error {
	if toolCallID == "" {
		return fmt.Errorf("engine: EmitStreamToolResult: toolCallID is required")
	}
	return EmitStreamDelta(ctx, pub, runID, actorID, StreamDeltaPayload{
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
// runID and actorID feed the subject builder; both are sanitised by
// [SanitiseID] so caller-supplied IDs cannot fragment the resulting
// subject. The envelope is stamped with HeaderRunID, HeaderActorID and
// (for parity with the executor) HeaderNodeID = actorID so subscribers
// that filter on headers behave identically whether the delta came
// from a built-in node or a custom emitter.
//
// Publish errors are returned to the caller (unlike the executor's
// fire-and-forget convention) so node authors can decide whether to
// retry or surface the failure; in practice most callers just discard
// the error because stream deltas are observability, not control flow.
func EmitStreamDelta(ctx context.Context, pub Publisher, runID, actorID string, payload StreamDeltaPayload) error {
	if pub == nil {
		return nil
	}
	if err := validateStreamDelta(payload); err != nil {
		return err
	}
	subject := SubjectStreamDelta(runID, actorID)
	env, err := event.NewEnvelope(ctx, subject, payload)
	if err != nil {
		return err
	}
	if runID != "" {
		env.SetRunID(runID)
	}
	if actorID != "" {
		env.SetActorID(actorID)
		// HeaderNodeID is set in addition to HeaderActorID for
		// header-routed subscribers that key off the node. Inside the
		// graph runner the two values coincide; custom emitters MAY
		// pass a distinct nodeID by populating Headers themselves
		// before publishing.
		env.SetNodeID(actorID)
	}
	return pub.Publish(ctx, env)
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
	case "":
		return fmt.Errorf("engine: stream delta requires Type")
	default:
		// Forward-compatible Type — accept it.
		return nil
	}
}
