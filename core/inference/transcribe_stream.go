package inference

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// FeedTranscription pumps a live part stream into an open transcription
// session. It is the stream adapter between the pull-based message.Stream
// transport and the session's provider-compatible Send(AudioChunk) surface:
//
//   - every item must be an AudioPart carrying inline bytes; each becomes
//     one AudioChunk with a monotonic Sequence;
//   - an item Format, when present, must match format — the session's
//     negotiated input format;
//   - stream EOF ends feeding normally (the caller then drains the session);
//   - any other Read error aborts the session via Interrupt (barge-in) and
//     is returned.
//
// Item-shape errors (wrong part kind, non-inline source, format mismatch)
// return without touching the session so the caller can decide whether to
// abort or repair the stream.
func FeedTranscription(
	ctx context.Context,
	session TranscriptionSession,
	format media.AudioFormat,
	stream message.Stream,
) error {
	if isNilValue(session) {
		return fmt.Errorf("feed transcription: session is nil")
	}
	if isNilValue(stream) {
		return fmt.Errorf("feed transcription: stream is nil")
	}
	var sequence uint64
	for {
		item, err := stream.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if interruptErr := session.Interrupt(); interruptErr != nil {
				return fmt.Errorf(
					"feed transcription: interrupt session after stream failure: %v (stream: %w)",
					interruptErr, err,
				)
			}
			return fmt.Errorf("feed transcription: stream: %w", err)
		}
		part, err := message.NormalizePart(item)
		if err != nil {
			return fmt.Errorf("feed transcription: stream item: %w", err)
		}
		audio, ok := part.(message.AudioPart)
		if !ok {
			return fmt.Errorf(
				"feed transcription: stream yielded %T, want audio part", part)
		}
		if audio.Source.Kind() != media.SourceInline {
			return fmt.Errorf(
				"feed transcription: stream audio must carry inline bytes")
		}
		if audio.Format != nil && *audio.Format != format {
			return fmt.Errorf(
				"feed transcription: stream audio format %v does not match session input format %v",
				*audio.Format, format,
			)
		}
		chunk := media.AudioChunk{
			Data:     audio.Source.Bytes(),
			Sequence: sequence,
		}
		sequence++
		if err := session.Send(ctx, chunk); err != nil {
			return fmt.Errorf("feed transcription: session send: %w", err)
		}
	}
}

// TranscribeStream opens a duplex transcription session against the model
// addressed by ref, feeds the live part stream into it, drains the session
// to EOF, and returns the final transcript. Callers that want partials as
// they arrive use TranscribeSession plus FeedTranscription directly. When
// the session supports explicit end-of-input (TranscriptionSessionFinisher),
// the stream's end is signaled before draining so continuous sessions
// terminate deterministically.
func (a *Assembly) TranscribeStream(
	ctx context.Context,
	ref ModelRef,
	req TranscriptionSessionRequest,
	stream message.Stream,
) (TranscriptionResponse, error) {
	session, err := a.TranscribeSession(ctx, ref, req)
	if err != nil {
		return TranscriptionResponse{}, err
	}
	if err := FeedTranscription(ctx, session, req.InputFormat, stream); err != nil {
		return TranscriptionResponse{}, err
	}
	if finisher, ok := session.(TranscriptionSessionFinisher); ok {
		if err := finisher.FinishInput(ctx); err != nil {
			return TranscriptionResponse{}, fmt.Errorf(
				"transcribe stream finish input: %w",
				err,
			)
		}
	}
	for {
		if _, err := session.Next(ctx); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return TranscriptionResponse{}, err
		}
	}
	return session.Result()
}
