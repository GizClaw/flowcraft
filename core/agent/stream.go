package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/GizClaw/flowcraft/core/event"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// ---------- StreamDelta emit ----------

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

// EmitStreamPart publishes one assistant-token delta on the canonical
// stream subject. See [EmitStreamDelta] for the stepActor format
// requirement.
//
// Use this from any node that produces incremental textual output —
// for example a custom RAG retriever streaming its working notes, or a
// post-processing node turning structured data into prose. content may
// be empty (callers that want "still alive" heartbeats should typically
// mark them differently); empty content is published as-is so the
// helper stays predictable.
// EmitStreamPart publishes one canonical output part — text, image,
// audio, video, file, data, tool_call, tool_result, or reasoning — as
// a stream delta. part must be non-nil and valid (see
// [message.Part.Validate]). See [EmitStreamDelta] for the stepActor
// format requirement.
func EmitStreamPart(ctx context.Context, pub Publisher, runID, stepActor string, part message.Part) error {
	return EmitStreamDelta(ctx, pub, runID, stepActor, StreamDeltaPayload{
		Type: StreamDeltaPart,
		Part: part,
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
// (graph runner: ".node.<nodeID>"; iterative engine: ".iter<N>"). Both
// runID and stepActor are sanitised by [SanitiseID] so caller-supplied
// values cannot fragment the resulting subject.
//
// The envelope is stamped with HeaderRunID. The agent identifier is
// derived from the stepActor segment ahead of any optional ".node." /
// ".iter" suffix — it goes onto HeaderAgentID. For header-routed
// subscribers that key off the node id, the HeaderNodeID is populated
// whenever stepActor carries the graph runner's
// "<agent>.node.<nodeID>" form so the two transports stay aligned.
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
	// Lineage projection: when the ambient run identity carries a
	// parent run or a creating tool call (e.g. a delegated subagent
	// started by a delegate tool call), stamp them onto the envelope
	// so consumers can build the run tree without a separate join.
	// Outside an engine run the ambient identity is absent and both
	// headers are simply skipped.
	if info, ok := RunInfoFromContext(ctx); ok {
		if info.ParentRunID != "" {
			env.SetParentRunID(info.ParentRunID)
		}
		if callID := info.Attribute(telemetry.AttrToolCallID); callID != "" {
			env.SetToolCallID(callID)
		}
	}
	return pub.Publish(ctx, env)
}

// splitStepActor extracts the agent.id prefix and the optional graph
// runner ".node.<nodeID>" suffix from a stepActor string. Returns
// (stepActor, "") when no recognised suffix is present, so engines
// that use a different suffix scheme (e.g. an iterative engine's ".iter<N>")
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
	case StreamDeltaPart:
		if partIsNil(p.Part) {
			return fmt.Errorf("engine: stream delta part requires a message part")
		}
		normalized, err := message.NormalizePart(p.Part)
		if err != nil {
			return fmt.Errorf("engine: stream delta part: %w", err)
		}
		if err := normalized.Validate(); err != nil {
			return fmt.Errorf("engine: stream delta part: %w", err)
		}
		if p.Reason != "" {
			return fmt.Errorf("engine: stream delta part must not carry Reason")
		}
		return validateStreamDataIdentity(p)
	case StreamDeltaFinish:
		if p.FinishReason == "" {
			return fmt.Errorf("engine: stream delta finish requires FinishReason")
		}
		if !partIsNil(p.Part) {
			return fmt.Errorf("engine: stream delta finish must not carry Part")
		}
		return validateStreamDataIdentity(p)
	case StreamDeltaProviderOutputs:
		if len(p.ProviderOutputs) == 0 {
			return fmt.Errorf("engine: stream delta provider_outputs requires ProviderOutputs")
		}
		for i, output := range p.ProviderOutputs {
			if output.Provider == "" {
				return fmt.Errorf("engine: stream delta provider_outputs[%d] requires Provider", i)
			}
			if output.Extension == "" {
				return fmt.Errorf("engine: stream delta provider_outputs[%d] requires Extension", i)
			}
			if len(output.Value) == 0 || !json.Valid(output.Value) {
				return fmt.Errorf("engine: stream delta provider_outputs[%d] has invalid Value", i)
			}
		}
		if !partIsNil(p.Part) {
			return fmt.Errorf("engine: stream delta provider_outputs must not carry Part")
		}
		return validateStreamDataIdentity(p)
	case StreamDeltaParallelBranchAccept, StreamDeltaParallelBranchCancel:
		if p.ForkID == "" {
			return fmt.Errorf("engine: stream delta %s requires ForkID", p.Type)
		}
		if p.BranchID == "" {
			return fmt.Errorf("engine: stream delta %s requires BranchID", p.Type)
		}
		if !partIsNil(p.Part) {
			return fmt.Errorf("engine: stream delta %s must not carry Part", p.Type)
		}
		return nil
	case "":
		return fmt.Errorf("engine: stream delta requires Type")
	default:
		// Forward-compatible Type — accept it.
		return nil
	}
}

// partIsNil reports whether part is nil or a typed-nil pointer behind
// the [message.Part] interface.
func partIsNil(part message.Part) bool {
	if part == nil {
		return true
	}
	switch v := reflect.ValueOf(part); v.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
		reflect.Pointer, reflect.Slice:
		return v.IsNil()
	}
	return false
}

// validateStreamDataIdentity keeps ordinary and speculative data deltas
// unambiguous. Speculative branch output always carries both correlation
// identifiers; non-speculative output carries neither.
func validateStreamDataIdentity(p StreamDeltaPayload) error {
	if p.Speculative {
		if p.ForkID == "" {
			return fmt.Errorf("engine: speculative stream delta %s requires ForkID", p.Type)
		}
		if p.BranchID == "" {
			return fmt.Errorf("engine: speculative stream delta %s requires BranchID", p.Type)
		}
		return nil
	}
	if p.ForkID != "" || p.BranchID != "" {
		return fmt.Errorf(
			"engine: non-speculative stream delta %s must not carry ForkID or BranchID",
			p.Type)
	}
	return nil
}

// StreamSink is the consumer-side counterpart of the EmitStream*
// helpers. A sink receives one decoded [StreamDeltaPayload] at a time
// along with its source envelope (for headers / trace ids / raw subject
// access) and forwards it to whatever transport the caller cares about.
//
// Implementations:
//   - MUST be safe for concurrent OnDelta calls;
//   - SHOULD return errors only for unrecoverable failures;
//   - MUST observe ctx.Done and return promptly.
type StreamSink interface {
	OnDelta(ctx context.Context, env event.Envelope, delta StreamDeltaPayload) error
}

// StreamSinkFunc is a func adapter for [StreamSink].
type StreamSinkFunc func(ctx context.Context, env event.Envelope, delta StreamDeltaPayload) error

// OnDelta implements StreamSink.
func (f StreamSinkFunc) OnDelta(ctx context.Context, env event.Envelope, delta StreamDeltaPayload) error {
	return f(ctx, env, delta)
}

var _ StreamSink = StreamSinkFunc(nil)
