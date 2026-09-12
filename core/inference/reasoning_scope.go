package inference

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
)

// Reasoning provenance. A reasoning trace is signed or encrypted by the
// provider that produced it, and only that provider — under the same account
// and address — can verify it again. Replaying a trace to a target that
// cannot verify it is a hard provider error that no retry and no fallback can
// repair, because the trace stays in the conversation. Drivers therefore
// stamp every trace they produce with a verification scope and replay one
// only when the target's scope matches.
//
// The scope is a driver-owned, opaque token on message.ReasoningPart.Source:
// this package defines how it is composed and copied, never what it means.

// ReasoningScope returns the verification scope of one addressed model: the
// token a driver stamps on the reasoning traces it produces and compares
// before replaying a stored one.
//
// declared is the deployment's own token (its provider spec), and replaces
// the derived value entirely: an operator who has verified that a set of
// models or credentials can verify each other's traces says so once and every
// model of that deployment shares the scope. Otherwise the scope is the
// address that produced the trace — provider, model, and credential profile —
// because a provider rejects a payload another account or model cannot
// decrypt, and the conservative answer is to replay nothing across them.
func ReasoningScope(declared, provider, model, profile string) string {
	if declared != "" {
		return declared
	}
	scope := provider + "/" + model
	if profile != "" {
		scope += "/" + profile
	}
	return scope
}

// WithReasoningSource stamps every reasoning part a decoder produced with
// source, so the attempt that replays the trace can tell whether this target
// may. It is the decode half of [ReasoningScope]; drivers wrap their decoders
// with it where the model reference and credential profile are known.
//
// A decode that fails passes its error through untouched: a failure has no
// parts to attribute.
func WithReasoningSource[Raw any](
	decode Decoder[Raw, GenerateResponse],
	source string,
) Decoder[Raw, GenerateResponse] {
	if source == "" || decode == nil {
		return decode
	}
	return func(ctx context.Context, raw Raw) (GenerateResponse, error) {
		response, err := decode(ctx, raw)
		if err != nil {
			return response, err
		}
		for index, part := range response.Message.Content.Parts {
			normalized, err := message.NormalizePart(part)
			if err != nil {
				continue
			}
			reasoning, ok := normalized.(message.ReasoningPart)
			if !ok {
				continue
			}
			reasoning.Source = source
			response.Message.Content.Parts[index] = reasoning
		}
		return response, nil
	}
}

// WithReasoningDeltaSource is [WithReasoningSource] for the streaming path:
// it stamps the reasoning deltas a stream decoder produced, and the stream
// accumulator carries the stamp onto the assembled part.
func WithReasoningDeltaSource[RawEvent any](
	decode GenerateStreamDecoder[RawEvent],
	source string,
) GenerateStreamDecoder[RawEvent] {
	if source == "" || decode == nil {
		return decode
	}
	return func(ctx context.Context, raw RawEvent) (GenerateStreamEvent, error) {
		event, err := decode(ctx, raw)
		if err != nil {
			return event, err
		}
		switch delta := event.Delta.(type) {
		case ReasoningDelta:
			delta.Source = source
			event.Delta = delta
		case *ReasoningDelta:
			if delta != nil {
				delta.Source = source
			}
		}
		return event, nil
	}
}
