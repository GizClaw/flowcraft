package agent

import (
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// EventKind names one event of a running Task.
type EventKind string

const (
	// KindStreamStart announces that a round opened a model stream.
	KindStreamStart EventKind = "stream-start"
	// KindText is appended answer text.
	KindText EventKind = "text"
	// KindReasoning is appended reasoning text.
	KindReasoning EventKind = "reasoning"
	// KindToolCall announces one tool call about to execute.
	KindToolCall EventKind = "tool-call"
	// KindToolResult carries the result of that call.
	KindToolResult EventKind = "tool-result"
	// KindArtifact announces a produced artifact reference.
	KindArtifact EventKind = "artifact"
	// KindUsage reports token accounting for one round.
	KindUsage EventKind = "usage"
	// KindStatus reports a lifecycle transition.
	KindStatus EventKind = "status"
	// KindSteerApplied reports queued steering that reached the model.
	KindSteerApplied EventKind = "steer-applied"
	// KindInterrupted reports that an interrupt cancelled the running round.
	KindInterrupted EventKind = "interrupted"
	// KindUndelivered reports input left in the mailbox when the Task ended.
	KindUndelivered EventKind = "undelivered"
)

// Event is one step of a running Task.
//
// The interface is sealed by an unexported method: only this package can
// implement it, so a Kind always names one of the types below and a consumer's
// type switch covers everything it can receive. Adding an event kind is an
// `agent` change on purpose — the wire vocabulary stays closed.
type Event interface {
	Kind() EventKind
	event()
}

// StreamStart announces one round's model call.
type StreamStart struct {
	Round int
	Model string
}

// Text appends answer text.
type Text struct{ Text string }

// Reasoning appends reasoning text.
type Reasoning struct{ Text string }

// ToolCallStart announces one tool call.
type ToolCallStart struct{ Call message.ToolCall }

// ToolResult carries one tool result.
type ToolResult struct{ Result message.ToolResult }

// ArtifactWritten announces a produced artifact.
type ArtifactWritten struct{ Artifact Artifact }

// UsageEvent reports one round's accounting.
type UsageEvent struct{ Usage usage.Usage }

// SteerApplied reports steering messages that were appended to the transcript
// and are visible to the next model request.
type SteerApplied struct {
	Count    int
	Messages []message.Message
}

// Interrupted reports that the running round was cancelled by an interrupt.
type Interrupted struct{ Reason string }

// Undelivered reports mailbox entries the Task did not consume. They are handed
// up rather than dropped: only the caller knows whether the conversation
// continues or the client is gone.
type Undelivered struct{ Messages []message.Message }

// Status reports a lifecycle transition.
type Status struct {
	State  TaskState
	Reason string
}

func (StreamStart) Kind() EventKind     { return KindStreamStart }
func (Text) Kind() EventKind            { return KindText }
func (Reasoning) Kind() EventKind       { return KindReasoning }
func (ToolCallStart) Kind() EventKind   { return KindToolCall }
func (ToolResult) Kind() EventKind      { return KindToolResult }
func (ArtifactWritten) Kind() EventKind { return KindArtifact }
func (UsageEvent) Kind() EventKind      { return KindUsage }
func (Status) Kind() EventKind          { return KindStatus }
func (SteerApplied) Kind() EventKind    { return KindSteerApplied }
func (Interrupted) Kind() EventKind     { return KindInterrupted }
func (Undelivered) Kind() EventKind     { return KindUndelivered }

// The seal. Every event kind is one of these types and nothing outside this
// package can add another.
func (StreamStart) event()     {}
func (Text) event()            {}
func (Reasoning) event()       {}
func (ToolCallStart) event()   {}
func (ToolResult) event()      {}
func (ArtifactWritten) event() {}
func (UsageEvent) event()      {}
func (SteerApplied) event()    {}
func (Interrupted) event()     {}
func (Undelivered) event()     {}
func (Status) event()          {}
