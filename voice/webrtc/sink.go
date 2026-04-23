package webrtc

import (
	"context"
	"io"
	"time"

	"github.com/GizClaw/flowcraft/sdk/telemetry"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/tts"
	"github.com/pion/webrtc/v4/pkg/media"
	"go.opentelemetry.io/otel/metric"
)

var (
	sinkEncodeDrops, _ = telemetry.Meter().Int64Counter("webrtc.sink.encode_drops_total",
		metric.WithDescription("Audio frames dropped due to encode failure"))
	sinkWriteDrops, _ = telemetry.Meter().Int64Counter("webrtc.sink.write_drops_total",
		metric.WithDescription("Audio frames dropped due to track write failure"))
)

// Sink implements voice.AudioSink by encoding PCM audio frames via the
// injected AudioEncoder and writing them to a local WebRTC audio track.
type Sink struct {
	track   trackWriter
	encoder AudioEncoder
}

// trackWriter is the subset of pion's TrackLocalStaticSample we need.
// Extracted as an interface for testability.
type trackWriter interface {
	WriteSample(s media.Sample) error
}

func newSink(track trackWriter, encoder AudioEncoder) *Sink {
	return &Sink{track: track, encoder: encoder}
}

// Play reads utterances from the stream, encodes the PCM audio to Opus, and
// writes the encoded packets to the WebRTC track. Implements voice.AudioSink.
//
// Stream termination semantics (matching AudioSink contract):
//   - io.EOF: turn ended normally — all audio has been written.
//   - Other error: interruption — stop immediately.
func (s *Sink) Play(stream audio.Stream[tts.Utterance]) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			utt, err := stream.Read()
			if err != nil {
				if err != io.EOF {
					s.encoder.Reset()
				}
				return
			}
			s.writeUttToTrack(utt)
		}
	}()
	return done
}

const opusFrameDuration = 20 * time.Millisecond

func (s *Sink) writeUttToTrack(utt tts.Utterance) {
	data := utt.Data
	if len(data) == 0 {
		return
	}

	sampleRate := utt.Format.SampleRate
	if sampleRate <= 0 {
		sampleRate = 48000
	}
	channels := utt.Format.Channels
	if channels <= 0 {
		channels = 1
	}
	bytesPerSample := 2
	samplesPerFrame := sampleRate * int(opusFrameDuration.Milliseconds()) / 1000
	frameBytes := samplesPerFrame * channels * bytesPerSample

	for len(data) > 0 {
		chunk := data
		if len(chunk) >= frameBytes {
			chunk = data[:frameBytes]
			data = data[frameBytes:]
		} else {
			padded := make([]byte, frameBytes)
			copy(padded, chunk)
			chunk = padded
			data = nil
		}

		encoded, err := s.encoder.Encode(chunk)
		if err != nil || len(encoded) == 0 {
			sinkEncodeDrops.Add(context.Background(), 1)
			continue
		}

		if err := s.track.WriteSample(media.Sample{
			Data:     encoded,
			Duration: opusFrameDuration,
		}); err != nil {
			sinkWriteDrops.Add(context.Background(), 1)
		}
	}
}
