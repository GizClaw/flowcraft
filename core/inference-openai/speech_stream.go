package openai

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/openai/openai-go/v3"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/inference/usage"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// speechChunkBytes is how much audio one chunk carries. It bounds the latency a
// player sees without making the per-chunk overhead dominate.
const speechChunkBytes = 32 << 10

// bodyChunks adapts a response body to the protocol's wire stream.
//
// It yields the body in fixed chunks, then one empty chunk, then the end. The
// empty chunk is how a per-chunk decoder learns that the body finished: a decoder
// is a plain function over one chunk and cannot otherwise emit the terminal
// events.
type bodyChunks struct {
	body   io.ReadCloser
	buffer []byte
	// endSent records that the empty marker chunk was delivered.
	endSent bool
	done    bool
}

func (s *bodyChunks) Next(ctx context.Context) ([]byte, error) {
	if s.done {
		return nil, io.EOF
	}
	if s.endSent {
		s.done = true
		return nil, io.EOF
	}
	for {
		if err := ctx.Err(); err != nil {
			s.done = true
			return nil, err
		}
		n, err := s.body.Read(s.buffer)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, s.buffer[:n])
			return chunk, nil
		}
		if errors.Is(err, io.EOF) {
			s.endSent = true
			return []byte{}, nil
		}
		if err != nil {
			s.done = true
			return nil, err
		}
	}
}

func (s *bodyChunks) Close() error { return s.body.Close() }

// speechOpen starts the streaming call. The provider answers with a chunked audio
// body, so the wire value is bytes.
func (d *driver) speechOpen(
	ctx context.Context,
	_ inference.GenerateRequest,
	params openai.AudioSpeechNewParams,
) (protocol.RawStream[[]byte], error) {
	response, err := d.client.Audio.Speech.New(ctx, params)
	if err != nil {
		return nil, err
	}
	return &bodyChunks{body: response.Body, buffer: make([]byte, speechChunkBytes)}, nil
}

// speechStreamDecoder maps audio bytes onto canonical events. Every chunk appends
// to content index zero, which is the semantics [inference.StreamChunk] names.
func speechStreamDecoder(request inference.GenerateRequest) protocol.StreamDecoder[[]byte] {
	options := speechOptions(request.Extensions)
	format := speechFormat(options)
	mediaType := speechMediaType(format)
	encoding, sampleRate, channels := speechEncoding(format)
	metadata := inference.Metadata{
		Model:     model.ModelID{Provider: ProviderID, Name: request.Model.ID.Name},
		Operation: inference.OperationGenerate,
	}
	characters := int64(len([]rune(request.Input.Content.Text())))

	started := false
	received := false
	return func(_ context.Context, chunk []byte) ([]inference.StreamEvent, error) {
		var events []inference.StreamEvent
		if !started {
			started = true
			events = append(events, inference.StreamEvent{
				Kind:     inference.StreamStart,
				Metadata: &metadata,
			})
		}
		if len(chunk) == 0 {
			if !received {
				return nil, inference.NewError(
					inference.ProviderResponseUnusable,
					inference.OperationGenerate,
					"",
					fmt.Errorf("openai: speech stream carried no audio"),
				)
			}
			accounting := usage.Usage{InputCharacters: &characters}
			events = append(events,
				inference.StreamEvent{Kind: inference.StreamUsage, Usage: &accounting},
				inference.StreamEvent{Kind: inference.StreamEnd, Finish: inference.FinishStop},
			)
			return events, nil
		}
		source, err := media.NewAudioBytes(chunk, mediaType)
		if err != nil {
			return nil, err
		}
		part := message.AudioPart{Source: source}
		if encoding != "" {
			part.Format = &media.AudioFormat{
				Encoding:     encoding,
				SampleRateHz: sampleRate,
				Channels:     channels,
			}
		}
		received = true
		events = append(events, inference.StreamEvent{
			Kind:  inference.StreamChunk,
			Index: imagePartIndex,
			Part:  part,
		})
		return events, nil
	}
}
