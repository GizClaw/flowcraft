package inference

import (
	"fmt"

	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
)

// StreamEventKind names one step of a streaming generation. A driver emits them
// in order; the order and the terminal conditions are the stream contract.
type StreamEventKind string

const (
	// StreamStart opens the stream. It carries the response metadata that is
	// known before any content arrives.
	StreamStart StreamEventKind = "start"
	// StreamText appends text to the assistant message.
	StreamText StreamEventKind = "text"
	// StreamReasoning appends reasoning text to the assistant message. It is
	// separate from StreamText so a caller can hide or store it without
	// parsing markers out of the visible answer.
	StreamReasoning StreamEventKind = "reasoning"
	// StreamToolCall appends a fragment of one tool call. Fragments for the
	// same call share an Index and arrive in order.
	StreamToolCall StreamEventKind = "tool_call"
	// StreamPart delivers a content part the stream cannot express as
	// incremental text: a generated image arrives as one complete part, and
	// synthesized audio arrives as a sequence of chunk parts. It is the same
	// event for every modality so that adding an output does not fork the
	// stream contract.
	StreamPart StreamEventKind = "part"
	// StreamChunk appends bytes to the media part at Index. The first chunk at an
	// index carries the part template — its media type and format — and later
	// chunks carry the bytes that follow. It exists because media payloads have
	// the opposite merge rule from previews: [StreamPart] replaces, a chunk
	// appends, and the difference is never inferred from the payload.
	StreamChunk StreamEventKind = "chunk"
	// StreamUsage reports interim accounting. Providers that only report
	// totals at the end emit it once, just before StreamEnd.
	StreamUsage StreamEventKind = "usage"
	// StreamEnd closes the stream and carries the finish reason. It is
	// always the last event.
	StreamEnd StreamEventKind = "end"
)

// Validate reports whether the kind is one the contract names.
func (k StreamEventKind) Validate() error {
	switch k {
	case StreamStart, StreamText, StreamReasoning, StreamToolCall, StreamPart,
		StreamChunk, StreamUsage, StreamEnd:
		return nil
	default:
		return fmt.Errorf("unknown stream event kind %q", k)
	}
}

// Stream is one in-flight generation: a [Pull] of events, plus the answer those
// events assemble into.
//
// Next follows the pull contract — [io.EOF] ends the stream, any other error is
// terminal, and the caller must then Close. Next must not be called concurrently
// with itself.
//
// Result returns the assembled response once Next has reported io.EOF. It is the
// same value a non-streaming call would have produced, which is what lets a caller
// that does not care about increments treat both paths alike.
type Stream interface {
	Pull[StreamEvent]
	Result() (GenerateResponse, error)
	Close() error
}

// ToolCallDelta is one fragment of a tool call under assembly. ID and Name are
// normally carried by the first fragment; Arguments accumulates across
// fragments. The fragment carries no index of its own: [StreamEvent.Index] is
// the content part index for every event kind, so an assembler never has to
// reconcile two index spaces.
type ToolCallDelta struct {
	// ID is the provider-assigned call id.
	ID string `json:"id,omitempty"`
	// Name is the tool being called.
	Name string `json:"name,omitempty"`
	// Arguments is the JSON argument fragment carried by this event.
	Arguments string `json:"arguments,omitempty"`
}

// StreamEvent is one step of a streaming generation. Only the fields the Kind
// calls for are set.
type StreamEvent struct {
	Kind StreamEventKind `json:"kind"`
	// Index is the content part index this event belongs to. It is the one
	// index space for every kind: a tool call is a part at its own index, not
	// a separate sequence.
	Index int `json:"index,omitempty"`
	// Text is the appended text for StreamText and StreamReasoning.
	Text string `json:"text,omitempty"`
	// Signature, ID and Source complete a reasoning block: providers that
	// expect their own reasoning to be replayed verbatim attach it to the
	// final fragment. They are ignored for every other kind.
	Signature string `json:"signature,omitempty"`
	ID        string `json:"id,omitempty"`
	Source    string `json:"source,omitempty"`
	// Tool is the fragment for StreamToolCall.
	Tool *ToolCallDelta `json:"tool,omitempty"`
	// Part is the content part for StreamPart.
	Part message.Part `json:"part,omitempty"`
	// Usage is the accounting for StreamUsage.
	Usage *usage.Usage `json:"usage,omitempty"`
	// Metadata is the response identity for StreamStart.
	Metadata *Metadata `json:"metadata,omitempty"`
	// Finish is the reason for StreamEnd.
	Finish FinishReason `json:"finish,omitempty"`
}

// Validate checks the event's own invariants: a kind that names its payload,
// and no payload for a kind that does not carry one.
func (e StreamEvent) Validate() error {
	if err := e.Kind.Validate(); err != nil {
		return err
	}
	switch e.Kind {
	case StreamStart:
		if e.Text != "" || e.Tool != nil || e.Part != nil || e.Usage != nil || e.Finish != "" {
			return fmt.Errorf("stream start event carries content")
		}
	case StreamText, StreamReasoning:
		if e.Tool != nil || e.Part != nil || e.Usage != nil || e.Finish != "" {
			return fmt.Errorf("%s event carries a foreign payload", e.Kind)
		}
	case StreamToolCall:
		if e.Tool == nil {
			return fmt.Errorf("stream tool_call event carries no fragment")
		}
		if e.Text != "" || e.Part != nil || e.Usage != nil || e.Finish != "" {
			return fmt.Errorf("stream tool_call event carries a foreign payload")
		}
	case StreamPart:
		if e.Part == nil {
			return fmt.Errorf("stream part event carries no part")
		}
		if e.Text != "" || e.Tool != nil || e.Usage != nil || e.Finish != "" {
			return fmt.Errorf("stream part event carries a foreign payload")
		}
	case StreamChunk:
		if e.Part == nil {
			return fmt.Errorf("stream chunk event carries no part")
		}
		switch e.Part.Kind() {
		case message.PartImage, message.PartAudio, message.PartVideo:
		default:
			return fmt.Errorf("stream chunk of %q has no bytes to append", e.Part.Kind())
		}
		if e.Text != "" || e.Tool != nil || e.Usage != nil || e.Finish != "" {
			return fmt.Errorf("stream chunk event carries a foreign payload")
		}
	case StreamUsage:
		if e.Usage == nil {
			return fmt.Errorf("stream usage event carries no usage")
		}
		if e.Text != "" || e.Tool != nil || e.Part != nil || e.Finish != "" {
			return fmt.Errorf("stream usage event carries a foreign payload")
		}
	case StreamEnd:
		if err := e.Finish.Validate(); err != nil {
			return err
		}
		if e.Text != "" || e.Tool != nil || e.Part != nil || e.Usage != nil {
			return fmt.Errorf("stream end event carries content")
		}
	}
	return nil
}
