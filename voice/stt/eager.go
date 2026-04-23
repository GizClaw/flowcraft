package stt

import (
	"context"
	"io"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/vad"
)

// EagerRecognizer wraps a non-streaming STT with a VAD to provide
// pseudo-streaming recognition via segment-based eager submission.
type EagerRecognizer struct {
	stt STT
	vad vad.VAD
}

func NewEagerRecognizer(s STT, v vad.VAD) *EagerRecognizer {
	return &EagerRecognizer{stt: s, vad: v}
}

// Recognize delegates to the inner STT.
func (e *EagerRecognizer) Recognize(ctx context.Context, input audio.Frame, opts ...STTOption) (STTResult, error) {
	return e.stt.Recognize(ctx, input, opts...)
}

// RecognizeStream implements StreamSTT by splitting audio via VAD.
// Input and output both use Stream[T].
//
// The returned output stream is bound to ctx: when ctx is cancelled the output
// pipe is interrupted so downstream consumers unblock immediately. The caller
// is responsible for closing/interrupting the input stream to allow the
// internal goroutine to exit.
func (e *EagerRecognizer) RecognizeStream(
	ctx context.Context,
	input audio.Stream[audio.Frame],
	opts ...STTOption,
) (audio.Stream[STTResult], error) {
	out := audio.NewPipe[STTResult](4)
	stop := context.AfterFunc(ctx, out.Interrupt)
	go func() {
		defer stop()
		defer out.Close()

		var accumulated []byte
		var format audio.Format
		hasFormat := false

		for {
			frame, err := input.Read()
			if err != nil {
				if err == io.EOF {
					if rest := e.vad.Flush(); len(rest) > 0 {
						accumulated = append(accumulated, rest...)
					}
					if len(accumulated) > 0 {
						inputFrame := audio.Frame{Data: accumulated, Format: format}
						result, recErr := e.stt.Recognize(ctx, inputFrame, opts...)
						if recErr != nil {
							return
						}
						buf := make([]byte, len(accumulated))
						copy(buf, accumulated)
						result.Audio = audio.Frame{Data: buf, Format: format}
						result.IsFinal = true
						out.Send(result)
					}
				}
				return
			}

			if !hasFormat {
				format = frame.Format
				hasFormat = true
				if sa, ok := e.vad.(vad.SampleRateAware); ok && format.SampleRate > 0 {
					sa.SetSampleRate(format.SampleRate)
				}
			}

			segment, isFinal := e.vad.Feed(frame.Data)
			if segment == nil {
				continue
			}
			accumulated = append(accumulated, segment...)
			inputFrame := audio.Frame{Data: accumulated, Format: format}
			result, err := e.stt.Recognize(ctx, inputFrame, opts...)
			if err != nil {
				continue
			}
			buf := make([]byte, len(accumulated))
			copy(buf, accumulated)
			result.Audio = audio.Frame{Data: buf, Format: format}
			result.IsFinal = isFinal
			if !out.Send(result) {
				return
			}
			if isFinal {
				accumulated = nil
				e.vad.Reset()
			}
		}
	}()
	return out, nil
}
