package message

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/message/media"
)

// defaultMaterializeMaxBytes bounds the inline bytes produced when a
// stream-backed audio/video part is drained into memory. A hostile or
// runaway producer cannot force an unbounded allocation before the
// part is validated downstream.
const defaultMaterializeMaxBytes = 64 << 20

// Stream is the canonical pull-based transport for live content: the
// message-level instantiation of [media.Stream] over [Part].
//
// A Stream is how a message "exists" while it is in flight — for example
// the audio/video input of a live session. It is deliberately not a new
// DTO: the items are ordinary [Part] values, and a stream-backed part is
// materialized back into ordinary parts before it enters durable context
// or history (see [MaterializeContent]).
type Stream = media.Stream[Part]

// Pipe is the bounded, buffered implementation of [Stream]. Producers Send
// into it (blocking when the buffer is full, which is the backpressure
// contract); consumers Read from it. See [media.Pipe] for the full
// Close / Interrupt semantics.
type Pipe = media.Pipe[Part]

// NewPartPipe creates a bounded part pipe with the given buffer capacity.
func NewPartPipe(bufferSize int) *Pipe {
	return media.NewPipe[Part](bufferSize)
}

// NewAudioStream wraps a live part stream as an audio source, the
// message-level convenience for [media.NewAudioStream] instantiated over
// [Part].
func NewAudioStream(stream Stream, mediaType string) (media.AudioSource, error) {
	return media.NewAudioStream(stream, mediaType)
}

// NewVideoStream wraps a live part stream as a video source, the
// message-level convenience for [media.NewVideoStream] instantiated over
// [Part].
func NewVideoStream(stream Stream, mediaType string) (media.VideoSource, error) {
	return media.NewVideoStream(stream, mediaType)
}

// HasStreamSource reports whether any part in content carries a
// stream-backed media source. It is the cheap pre-check callers use before
// deciding whether [MaterializeContent] has work to do.
func HasStreamSource(content Content) bool {
	for _, part := range content.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			continue
		}
		switch value := normalized.(type) {
		case AudioPart:
			if value.Source.Kind() == media.SourceStream {
				return true
			}
		case VideoPart:
			if value.Source.Kind() == media.SourceStream {
				return true
			}
		}
	}
	return false
}

// MaterializeContent converts every stream-backed audio and video part in
// content into its durable form by draining the stream to completion:
//   - a stream-backed AudioPart becomes one inline-byte AudioPart;
//   - a stream-backed VideoPart becomes one inline-byte VideoPart.
//
// Content without stream sources is returned unchanged. The stream is
// consumed exactly once: callers that also read it live must materialize
// from their own accumulated bytes instead (or ensure the stream is still
// readable here).
func MaterializeContent(ctx context.Context, content Content) (Content, error) {
	if !HasStreamSource(content) {
		return content, nil
	}
	out := make([]Part, 0, len(content.Parts))
	for _, part := range content.Parts {
		normalized, err := NormalizePart(part)
		if err != nil {
			return Content{}, fmt.Errorf("materialize content: %w", err)
		}
		materialized, err := MaterializePart(ctx, normalized)
		if err != nil {
			return Content{}, err
		}
		out = append(out, materialized...)
	}
	return Content{Parts: out}, nil
}

// MaterializePart returns part in its durable form. Stream-backed audio
// and video parts are drained into inline-byte parts; every other part is
// returned unchanged. See [MaterializeContent] for the rules.
func MaterializePart(ctx context.Context, part Part) ([]Part, error) {
	normalized, err := NormalizePart(part)
	if err != nil {
		return nil, err
	}
	switch value := normalized.(type) {
	case AudioPart:
		return materializeAudioPart(ctx, value)
	case VideoPart:
		return materializeVideoPart(ctx, value)
	default:
		return []Part{normalized}, nil
	}
}

func materializeAudioPart(ctx context.Context, part AudioPart) ([]Part, error) {
	if part.Source.Kind() != media.SourceStream {
		return []Part{part}, nil
	}
	stream, ok := part.Source.Stream().(Stream)
	if !ok {
		return nil, fmt.Errorf(
			"materialize audio: stream source carries %T, want message.Stream",
			part.Source.Stream(),
		)
	}
	var data bytes.Buffer
	var format *media.AudioFormat
	for {
		item, err := stream.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("materialize audio: %w", err)
		}
		audio, ok := item.(AudioPart)
		if !ok {
			return nil, fmt.Errorf(
				"materialize audio: stream yielded %T, want audio part", item)
		}
		if audio.Source.Kind() != media.SourceInline {
			return nil, fmt.Errorf(
				"materialize audio: stream part must carry inline bytes")
		}
		chunk := audio.Source.Bytes()
		if data.Len()+len(chunk) > defaultMaterializeMaxBytes {
			return nil, fmt.Errorf(
				"materialize audio: stream exceeds %d bytes", defaultMaterializeMaxBytes)
		}
		data.Write(chunk)
		if audio.Format == nil {
			continue
		}
		if format == nil {
			copied := *audio.Format
			format = &copied
			continue
		}
		if *format != *audio.Format {
			return nil, fmt.Errorf(
				"materialize audio: stream mixes formats %v and %v",
				*format, *audio.Format)
		}
	}
	if data.Len() == 0 {
		return nil, fmt.Errorf("materialize audio: stream produced no audio data")
	}
	// Stream items may omit the format when the enclosing part declared it.
	if format == nil {
		format = clonePointer(part.Format)
	}
	mediaType := ""
	if format != nil {
		mediaType = format.Encoding.MediaType()
	}
	if mediaType == "" {
		mediaType = part.Source.MediaType()
	}
	source, err := media.NewAudioBytes(data.Bytes(), mediaType)
	if err != nil {
		return nil, fmt.Errorf("materialize audio: %w", err)
	}
	duration := part.DurationMillis
	if duration == nil && format != nil {
		if millis, ok := media.AudioDurationMillis(data.Bytes(), *format); ok {
			duration = &millis
		}
	}
	materialized := AudioPart{
		Source:         source,
		Format:         format,
		DurationMillis: duration,
	}
	if err := materialized.Validate(); err != nil {
		return nil, fmt.Errorf("materialize audio: %w", err)
	}
	return []Part{materialized}, nil
}

func materializeVideoPart(ctx context.Context, part VideoPart) ([]Part, error) {
	if part.Source.Kind() != media.SourceStream {
		return []Part{part}, nil
	}
	stream, ok := part.Source.Stream().(Stream)
	if !ok {
		return nil, fmt.Errorf(
			"materialize video: stream source carries %T, want message.Stream",
			part.Source.Stream(),
		)
	}
	var data bytes.Buffer
	for {
		item, err := stream.Read(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("materialize video: %w", err)
		}
		video, ok := item.(VideoPart)
		if !ok {
			return nil, fmt.Errorf(
				"materialize video: stream yielded %T, want video part", item)
		}
		if video.Source.Kind() != media.SourceInline {
			return nil, fmt.Errorf(
				"materialize video: stream part must carry inline bytes")
		}
		chunk := video.Source.Bytes()
		if data.Len()+len(chunk) > defaultMaterializeMaxBytes {
			return nil, fmt.Errorf(
				"materialize video: stream exceeds %d bytes", defaultMaterializeMaxBytes)
		}
		data.Write(chunk)
	}
	if data.Len() == 0 {
		return nil, fmt.Errorf("materialize video: stream produced no video data")
	}
	source, err := media.NewVideoBytes(data.Bytes(), part.Source.MediaType())
	if err != nil {
		return nil, fmt.Errorf("materialize video: %w", err)
	}
	materialized := VideoPart{Source: source}
	if err := materialized.Validate(); err != nil {
		return nil, fmt.Errorf("materialize video: %w", err)
	}
	return []Part{materialized}, nil
}
