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
)

// Event is one step of a running Task.
//
// The interface has an unexported method on purpose: the set is closed, so a
// consumer can switch over every kind and the compiler keeps that switch honest
// when a kind is added.
type Event interface {
	Kind() EventKind
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
