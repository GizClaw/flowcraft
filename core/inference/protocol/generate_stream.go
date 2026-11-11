package protocol

import (
	"context"
	"errors"
	"io"

	"github.com/GizClaw/flowcraft/core/inference"
)

// RawStream is one in-flight provider stream at the wire level: the provider's
// own chunk type, yielded until the provider stops. It is the only streaming
// stage allowed to perform I/O.
//
// It follows the same pull contract as the host face ([inference.Pull]): [io.EOF]
// ends the stream cleanly, any other error is terminal, and the caller must not
// read again after either.
//
// Implementations must be safe for concurrent use.
type RawStream[Raw any] interface {
	inference.Pull[Raw]
	Close() error
}

// StreamOpener performs the streaming call and returns the wire-level stream. It
// receives the canonical request for the same reason [Transport] does.
type StreamOpener[Req Request, Wire, Raw any] func(
	ctx context.Context,
	request Req,
	wire Wire,
) (RawStream[Raw], error)

// StreamDecoder turns one wire chunk into zero or more canonical events. A chunk
// may carry no event at all (a keep-alive, an empty delta).
//
// Implementations must be safe for concurrent use.
type StreamDecoder[Raw any] func(ctx context.Context, raw Raw) ([]inference.StreamEvent, error)

// StreamDecoderFactory builds one decoder per stream. A server-sent-event stream
// is stateful — the opening chunk carries the response identity, tool-call
// fragments accumulate across chunks — so the decoder needs state that is
// private to one call rather than shared by every call on the driver.
//
// It receives the request for the same reason [Decoder] does: some streams never
// name the model or the media type they are producing.
type StreamDecoderFactory[Req Request, Raw any] func(
	request Req,
) StreamDecoder[Raw]

// GenerateStreamDriver is one bound streaming pipeline.
type GenerateStreamDriver interface {
	Open(ctx context.Context, request inference.GenerateRequest) (inference.Stream, error)
}

// BindGenerateStream assembles one streaming pipeline. Every stage is required,
// and so is the provider identity, for the same reason as [BindGenerate].
func BindGenerateStream[Wire, Raw any](
	providerID string,
	compile Compiler[inference.GenerateRequest, Wire],
	open StreamOpener[inference.GenerateRequest, Wire, Raw],
	newDecoder StreamDecoderFactory[inference.GenerateRequest, Raw],
) (GenerateStreamDriver, error) {
	switch {
	case providerID == "":
		return nil, errors.New("bind generate stream: provider id is required")
	case compile == nil:
		return nil, errors.New("bind generate stream: compiler is required")
	case open == nil:
		return nil, errors.New("bind generate stream: opener is required")
	case newDecoder == nil:
		return nil, errors.New("bind generate stream: decoder factory is required")
	}
	return &streamDriver[Wire, Raw]{
		providerID: providerID,
		compile:    compile,
		open:       open,
		newDecoder: newDecoder,
		observe:    generateObservation(),
	}, nil
}

type streamDriver[Wire, Raw any] struct {
	providerID string
	compile    Compiler[inference.GenerateRequest, Wire]
	open       StreamOpener[inference.GenerateRequest, Wire, Raw]
	newDecoder StreamDecoderFactory[inference.GenerateRequest, Raw]
	observe    observation[inference.GenerateRequest, inference.GenerateResponse]
}

// Open validates, compiles, starts the call and returns the ready stream,
// instrumented as one provider round trip.
//
// The span outlives Open: the response envelope and the usage only exist once the
// stream reports its result, so the returned stream carries the call and closes
// it on its terminal event.
func (d *streamDriver[Wire, Raw]) Open(
	ctx context.Context,
	request inference.GenerateRequest,
) (inference.Stream, error) {
	ctx, call := startCall(ctx, d.observe, d.providerID, request, ShapeStream)
	stream, err := d.start(ctx, request, call)
	if err != nil {
		call.finish(inference.GenerateResponse{}, err)
		return nil, err
	}
	call.logOpen()
	if call == nil {
		return stream, nil
	}
	return &instrumentedStream{inner: stream, call: call}, nil
}

// start opens the provider stream without instrumentation.
func (d *streamDriver[Wire, Raw]) start(
	ctx context.Context,
	request inference.GenerateRequest,
	call *call[inference.GenerateResponse],
) (inference.Stream, error) {
	request.Extensions = request.Extensions.ForProvider(d.providerID)
	compiled, err := prepare(ctx, d.providerID, d.compile, request, ShapeStream)
	if err != nil {
		return nil, err
	}
	if call != nil {
		call.attempted = true
	}
	raw, err := d.open(ctx, request, compiled.Wire)
	if err != nil {
		return nil, transportError(ctx, err)
	}
	if raw == nil {
		return nil, inference.NewError(
			inference.InvalidProviderResponse,
			"",
			errors.New("opener returned a nil stream"),
		)
	}
	decode := d.newDecoder(request)
	if decode == nil {
		return nil, inference.NewError(
			inference.InvalidProviderResponse,
			"",
			errors.New("decoder factory returned nil"),
		)
	}
	return &boundStream[Raw]{
		raw:      raw,
		decode:   decode,
		acc:      newAccumulator(),
		warnings: compiled.Warnings,
	}, nil
}

// boundStream relays decoded events one at a time and assembles the final
// response as they pass.
type boundStream[Raw any] struct {
	raw      RawStream[Raw]
	decode   StreamDecoder[Raw]
	acc      *accumulator
	warnings []inference.Warning

	pending []inference.StreamEvent
	ended   bool // the provider's wire stream is exhausted
	closed  bool
}

// Next returns the next event, or [io.EOF] once the stream ended cleanly.
func (s *boundStream[Raw]) Next(ctx context.Context) (inference.StreamEvent, error) {
	for len(s.pending) == 0 {
		if s.ended {
			return inference.StreamEvent{}, io.EOF
		}
		raw, err := s.raw.Next(ctx)
		if errors.Is(err, io.EOF) {
			s.ended = true
			if !s.acc.finished() {
				return inference.StreamEvent{}, inference.NewError(
					inference.ProviderTruncated,
					inference.FieldStream,
					errors.New("stream ended without a terminal event"),
				).WithDetail("stream.finish.missing")
			}
			return inference.StreamEvent{}, io.EOF
		}
		if err != nil {
			s.ended = true
			return inference.StreamEvent{}, transportError(ctx, err)
		}
		events, err := s.decode(ctx, raw)
		if err != nil {
			s.ended = true
			return inference.StreamEvent{}, decodeError(err)
		}
		for _, event := range events {
			if err := event.Validate(); err != nil {
				s.ended = true
				return inference.StreamEvent{}, inference.NewError(
					inference.InvalidProviderResponse,
					inference.FieldStream,
					err,
				)
			}
		}
		s.acc.add(events)
		s.pending = events
	}

	event := s.pending[0]
	s.pending = s.pending[1:]
	// A terminal event records the finish reason; it does not end the stream.
	// Providers routinely send accounting after it — the chat wire reports
	// usage in a final chunk with no choices — so the stream ends when the
	// provider stops sending, and the runner requires that a terminal event
	// arrived before reporting a clean end.
	return event, nil
}

// Result returns the assembled response. It is only meaningful after Next has
// reported [io.EOF]; before that the answer is incomplete and reported as
// truncated rather than returned half-built.
func (s *boundStream[Raw]) Result() (inference.GenerateResponse, error) {
	if !s.acc.finished() {
		return inference.GenerateResponse{}, inference.NewError(
			inference.ProviderTruncated,
			inference.FieldStream,
			errors.New("result requested before the stream finished"),
		).WithDetail("stream.result.incomplete")
	}
	response, err := s.acc.response()
	if err != nil {
		// A part that cannot be rebuilt means the driver emitted chunks whose
		// template is unusable; the answer is not salvageable.
		return inference.GenerateResponse{}, inference.NewError(
			inference.InvalidProviderResponse,
			inference.FieldStream,
			err,
		)
	}
	response.Warnings = joinWarnings(s.warnings, response.Warnings)
	return response, nil
}

// Close releases the wire stream. It is safe to call more than once.
func (s *boundStream[Raw]) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.ended = true
	s.pending = nil
	return s.raw.Close()
}
