package engine

import (
	"encoding/json"
	"fmt"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/event"
)

// This file defines the cross-engine event subject convention.
//
// Why subjects live in sdk/engine
//
// engine is the smallest layer every concrete execution engine must
// import (to satisfy [Engine.Execute]). Putting the subject convention
// here means:
//
//   - engine implementations have a single source of truth for "how to
//     name an event"; they MUST construct envelopes via the builders
//     below rather than fmt.Sprintf-ing their own strings;
//   - engine consumers (voice, SSE bridges, dashboards, kanban hooks)
//     can route on subject without knowing which engine produced the
//     event — they import sdk/engine, not the engine implementation.
//
// What this file does NOT lock down
//
// engine reserves only the subject prefixes documented below. A
// concrete engine MAY publish additional subjects under
// "engine.run.<runID>.<engine-private-segment>...". Examples in
// graph runner: ".parallel.fork", ".step.<id>.skipped". These extensions
// share the engine.run.<runID>. prefix so a single PatternRun
// subscription captures both the contract events and the engine's own
// extras, but the engine package does not standardise their shape.
//
// Subject schema (REQUIRED for every engine implementation):
//
//	engine.run.<runID>.start
//	engine.run.<runID>.end
//	engine.run.<runID>.step.<stepActor>.start
//	engine.run.<runID>.step.<stepActor>.complete
//	engine.run.<runID>.step.<stepActor>.error
//	engine.run.<runID>.stream.<stepActor>.delta
//
// "step" is the engine-neutral name for one unit of work in a run.
// "stepActor" identifies one such unit; it MUST start with the
// agent.id of the executing agent and MAY append an engine-private
// suffix to disambiguate units within the same agent run:
//
//   - graph runner: "<agent.id>.node.<node id>"
//   - vessel inline: "<agent.id>.iter<N>"
//   - script engine (future): "<agent.id>.stmt<N>"
//
// Engines are responsible for keeping the value dot/wildcard-free
// (use [SanitiseID]); SanitiseID also collapses any literal '.'
// inside the suffix into '_', so the resulting NATS segment is one
// flat token (e.g. "<agent>_node_<node>") rather than separate
// segments. This means agent-level fan-in via NATS subject
// wildcards is NOT supported on the bare segment — it would require
// schema-revising "step" into multiple segments, which the engine
// contract avoids to keep the wildcard surface stable. Consumers
// that want "show me only agent X's events" filter on the
// [event.HeaderAgentID] envelope header instead — that header is
// stamped by every engine alongside the subject and survives the
// segment collapse.
//
// "stream" is intentionally a sibling of "step" rather than a child:
// consumers that only care about LLM token / tool deltas (voice TTS,
// SSE token typewriter) can subscribe with [PatternRunStream] without
// also matching every step lifecycle event.

// SubjectPrefix is the fixed root every engine envelope subject MUST
// start with. Exposed as a constant so consumers can check
// strings.HasPrefix without re-deriving it.
const SubjectPrefix = "engine.run."

// ---------- Builders ----------

// SubjectRunStart returns the subject every engine MUST publish exactly
// once when [Engine.Execute] begins.
//
//	engine.run.<runID>.start
func SubjectRunStart(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.start", SubjectPrefix, SanitiseID(runID)))
}

// SubjectRunEnd returns the subject every engine MUST publish exactly
// once when [Engine.Execute] returns, regardless of outcome (clean
// completion, interrupt, cancel, failure).
//
//	engine.run.<runID>.end
func SubjectRunEnd(runID string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.end", SubjectPrefix, SanitiseID(runID)))
}

// SubjectStepStart returns the subject every engine MUST publish when
// it begins executing one step. stepActor identifies the unit of
// work; it MUST start with the executing agent.id (so consumers can
// reconstruct the agent identity from the subject when the envelope
// header is unavailable). See the file header for the per-engine
// suffix conventions (graph: ".node.<id>"; vessel inline:
// ".iter<N>") and for why agent-level NATS wildcard fan-in goes
// through the [event.HeaderAgentID] header instead of the subject.
//
//	engine.run.<runID>.step.<stepActor>.start
func SubjectStepStart(runID, stepActor string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.step.%s.start", SubjectPrefix, SanitiseID(runID), SanitiseID(stepActor)))
}

// SubjectStepComplete returns the subject every engine MUST publish
// when one step finishes successfully. See [SubjectStepStart] for
// the stepActor format.
//
//	engine.run.<runID>.step.<stepActor>.complete
func SubjectStepComplete(runID, stepActor string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.step.%s.complete", SubjectPrefix, SanitiseID(runID), SanitiseID(stepActor)))
}

// SubjectStepError returns the subject every engine MUST publish when
// one step fails (i.e. when it would normally cause Execute to return
// a non-nil non-interrupt error). See [SubjectStepStart] for the
// stepActor format.
//
//	engine.run.<runID>.step.<stepActor>.error
func SubjectStepError(runID, stepActor string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.step.%s.error", SubjectPrefix, SanitiseID(runID), SanitiseID(stepActor)))
}

// SubjectStreamDelta returns the subject every engine MUST use when
// emitting an in-flight increment from the step identified by
// stepActor — the canonical example is one LLM token, one dispatched
// tool call, or one tool result. See [SubjectStepStart] for the
// stepActor format.
//
// Payload MUST decode to a [StreamDeltaPayload]; see its docs for the
// per-Type field requirements.
//
//	engine.run.<runID>.stream.<stepActor>.delta
func SubjectStreamDelta(runID, stepActor string) event.Subject {
	return event.Subject(fmt.Sprintf("%s%s.stream.%s.delta", SubjectPrefix, SanitiseID(runID), SanitiseID(stepActor)))
}

// ---------- Patterns ----------

// PatternRun returns the wildcard pattern matching every event of one
// run, regardless of engine implementation or engine-private extension.
//
//	engine.run.<runID>.>
func PatternRun(runID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.>", SubjectPrefix, SanitiseID(runID)))
}

// PatternAllRuns returns the wildcard pattern matching every event from
// every run.
//
//	engine.run.>
func PatternAllRuns() event.Pattern {
	return event.Pattern(SubjectPrefix + ">")
}

// PatternRunSteps returns the wildcard pattern matching every step
// lifecycle event (start / complete / error and any engine-private
// step.* extension such as graph runner's "skipped") of one run.
//
//	engine.run.<runID>.step.>
func PatternRunSteps(runID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.step.>", SubjectPrefix, SanitiseID(runID)))
}

// PatternRunStream returns the wildcard pattern matching every stream
// delta of one run. Use this when you want LLM token / tool deltas but
// not the step lifecycle events.
//
// Agent-level fan-in ("only agent X's events in this run") is NOT
// available as a NATS wildcard because the stepActor segment is
// collapsed into one token by [SanitiseID] (see the file header).
// Consumers route by agent through the [event.HeaderAgentID]
// envelope header instead — subscribe with PatternRun(runID) and
// filter on env.AgentID() in the consumer.
//
//	engine.run.<runID>.stream.>
func PatternRunStream(runID string) event.Pattern {
	return event.Pattern(fmt.Sprintf("%s%s.stream.>", SubjectPrefix, SanitiseID(runID)))
}

// ---------- Classification helpers ----------

// IsStreamDelta reports whether s is a stream-delta subject. Cheap
// (string-only) so consumers can filter envelopes before the more
// expensive payload decode.
//
// Implementation note: matches subjects shaped like
// "engine.run.<runID>.stream.<stepActor>.delta" — i.e. the prefix
// is SubjectPrefix, contains ".stream." and ends with ".delta".
func IsStreamDelta(s event.Subject) bool {
	str := string(s)
	if len(str) < len(SubjectPrefix) || str[:len(SubjectPrefix)] != SubjectPrefix {
		return false
	}
	const tail = ".delta"
	if len(str) <= len(tail) || str[len(str)-len(tail):] != tail {
		return false
	}
	// Cheap "contains .stream." check without splitting; subjects with
	// a literal ".stream." in a stepActor are rejected by SanitiseID
	// before they reach this point.
	for i := len(SubjectPrefix); i+len(".stream.") <= len(str)-len(tail); i++ {
		if str[i:i+len(".stream.")] == ".stream." {
			return true
		}
	}
	return false
}

// ---------- Stream delta payload schema ----------

// StreamDeltaType enumerates the kinds of in-flight increments a stream
// envelope can carry. Engines MUST set [StreamDeltaPayload.Type] to one
// of these values; consumers SHOULD treat unknown values as forward-
// compatible additions and skip them.
type StreamDeltaType string

const (
	// StreamDeltaToken is one piece of generated assistant text.
	// Required field: Content.
	StreamDeltaToken StreamDeltaType = "token"

	// StreamDeltaToolCall is one tool invocation the model just
	// requested. Required fields: ID, Name. Recommended: Arguments.
	StreamDeltaToolCall StreamDeltaType = "tool_call"

	// StreamDeltaToolResult is the outcome of one tool invocation —
	// either the actual result, or a synthesised cancellation when
	// the round was interrupted before the call dispatched.
	// Required fields: ToolCallID, Content. Recommended: Name,
	// IsError, Cancelled.
	StreamDeltaToolResult StreamDeltaType = "tool_result"

	// StreamDeltaParallelBranchAccept marks a speculative parallel
	// branch's stream output as accepted by the graph runner.
	// Required fields: ForkID, BranchID.
	StreamDeltaParallelBranchAccept StreamDeltaType = "parallel_branch_accept"

	// StreamDeltaParallelBranchCancel marks a speculative parallel
	// branch's stream output as canceled/rolled back by the graph runner.
	// Required fields: ForkID, BranchID. Recommended: Reason.
	StreamDeltaParallelBranchCancel StreamDeltaType = "parallel_branch_cancel"
)

// StreamDeltaPayload is the canonical decoded shape of a
// [SubjectStreamDelta] envelope's payload.
//
// Engines MUST emit payloads that JSON-decode into this struct; the
// runtime constraint is checked by [DecodeStreamDelta]. Engines MAY
// add fields beyond what this struct lists — the JSON decoder is
// permissive on unknowns — but consumers SHOULD only rely on the
// fields documented here.
//
// Per-Type field requirements:
//
//	Type           Required               Recommended
//	------------   --------------------   --------------------
//	token          Content                —
//	tool_call      ID, Name               Arguments
//	tool_result    ToolCallID, Content    Name, IsError, Cancelled
//	parallel_branch_accept ForkID, BranchID —
//	parallel_branch_cancel ForkID, BranchID Reason
type StreamDeltaPayload struct {
	// Type discriminates the payload variant. See StreamDeltaType
	// constants for the standard values.
	Type StreamDeltaType `json:"type"`

	// Content carries the assistant text for "token" and the tool
	// output (typically already serialised) for "tool_result".
	Content string `json:"content,omitempty"`

	// ID is the tool-call identifier the model assigned. Set on
	// "tool_call" only; for "tool_result" use ToolCallID instead.
	ID string `json:"id,omitempty"`

	// Name is the tool name. Set on "tool_call"; recommended on
	// "tool_result" so consumers can correlate without a separate
	// dispatch table.
	Name string `json:"name,omitempty"`

	// Arguments is the tool input the model produced. Engines MAY
	// pass it as either a string (raw JSON) or an already-decoded
	// map / slice — consumers should accept both.
	Arguments any `json:"arguments,omitempty"`

	// ToolCallID pairs a "tool_result" with the originating
	// "tool_call". Required on tool_result.
	ToolCallID string `json:"tool_call_id,omitempty"`

	// IsError reports whether the tool dispatch returned an error
	// payload (vs. a successful result). Set on "tool_result".
	IsError bool `json:"is_error,omitempty"`

	// Cancelled reports whether this tool_result is a synthesised
	// cancellation (the call was never dispatched because the round
	// was interrupted). Set on "tool_result" only.
	Cancelled bool `json:"cancelled,omitempty"`

	// Speculative reports whether the delta belongs to a parallel branch
	// whose output has not yet been accepted into the parent board.
	Speculative bool `json:"speculative,omitempty"`

	// ForkID identifies the graph-runner parallel fork for speculative
	// branch deltas and parallel_branch_* control deltas.
	ForkID string `json:"fork_id,omitempty"`

	// BranchID identifies the branch within ForkID for speculative
	// branch deltas and parallel_branch_* control deltas.
	BranchID string `json:"branch_id,omitempty"`

	// Reason carries the rollback/abort reason for parallel_branch_cancel.
	Reason string `json:"reason,omitempty"`
}

// DecodeStreamDelta extracts the payload of a stream-delta envelope.
// It returns an error when the envelope payload is empty or does not
// JSON-decode into [StreamDeltaPayload]. It does NOT verify the
// subject; callers may pre-filter with [IsStreamDelta] when iterating
// a mixed stream.
func DecodeStreamDelta(env event.Envelope) (StreamDeltaPayload, error) {
	var p StreamDeltaPayload
	if len(env.Payload) == 0 {
		return p, errdefs.Validationf("engine: stream delta envelope %q has empty payload", env.Subject)
	}
	if err := json.Unmarshal(env.Payload, &p); err != nil {
		return p, errdefs.Validation(fmt.Errorf("engine: decode stream delta payload for %q: %w", env.Subject, err))
	}
	return p, nil
}

// ---------- Subject helpers ----------

// SanitiseID escapes characters that would corrupt an event.Subject
// when the input is concatenated into one. event.Subject segments are
// separated by '.', and '*' / '>' are reserved by event.Pattern for
// wildcards; any of these in a runID / stepActor would either fragment
// the subject or turn it into an unintended pattern. SanitiseID
// replaces each occurrence with '_'.
//
// Empty input becomes "_" so the resulting subject keeps a constant
// segment count even when the engine forgot to mint an id.
//
// Engines are expected to call SanitiseID on every user-supplied
// fragment they splice into a subject. The Subject* / Pattern*
// builders in this file already do so for their parameters; engine
// implementations only need it when constructing private extensions
// of their own.
func SanitiseID(id string) string {
	if id == "" {
		return "_"
	}
	out := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		switch id[i] {
		case '.', '*', '>':
			out = append(out, '_')
		default:
			out = append(out, id[i])
		}
	}
	return string(out)
}
