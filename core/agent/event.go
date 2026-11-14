package agent

import (
	"time"

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
	// KindInterrupted reports an applied (or accepted) interruption.
	KindInterrupted EventKind = "interrupted"
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
type ToolCallStart struct {
	Call message.ToolCall
	// ArgsHash is the call's canonical argument hash: the trace keeps the
	// identity, not the payload.
	ArgsHash string
}

// ToolResult carries one tool result.
type ToolResult struct {
	Result message.ToolResult
	// ResultHash is the result's canonical hash.
	ResultHash string
	// Duration is how long the call took.
	Duration time.Duration
	// ErrorKind classifies a failed result; empty for success.
	ErrorKind string
}

// ArtifactWritten announces a produced artifact.
type ArtifactWritten struct{ Artifact Artifact }

// UsageEvent reports one round's accounting.
type UsageEvent struct{ Usage usage.Usage }

// Status reports a lifecycle transition.
type Status struct {
	State  TaskState
	Reason string
}

// Interrupted reports an interruption the engine applied at a boundary.
type Interrupted struct{ Interruption }

func (StreamStart) Kind() EventKind     { return KindStreamStart }
func (Text) Kind() EventKind            { return KindText }
func (Reasoning) Kind() EventKind       { return KindReasoning }
func (ToolCallStart) Kind() EventKind   { return KindToolCall }
func (ToolResult) Kind() EventKind      { return KindToolResult }
func (ArtifactWritten) Kind() EventKind { return KindArtifact }
func (UsageEvent) Kind() EventKind      { return KindUsage }
func (Status) Kind() EventKind          { return KindStatus }
func (Interrupted) Kind() EventKind     { return KindInterrupted }

// The seal. Every event kind is one of these types and nothing outside this
// package can add another.
func (StreamStart) event()     {}
func (Text) event()            {}
func (Reasoning) event()       {}
func (ToolCallStart) event()   {}
func (ToolResult) event()      {}
func (ArtifactWritten) event() {}
func (UsageEvent) event()      {}
func (Status) event()          {}
func (Interrupted) event()     {}
