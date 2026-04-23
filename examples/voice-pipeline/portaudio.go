package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"

	"github.com/gordonklaus/portaudio"
	"github.com/hajimehoshi/go-mp3"

	"github.com/GizClaw/flowcraft/voice"
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/tts"
)

const (
	sampleRate      = 16000
	numChannels     = 1
	framesPerBuffer = sampleRate / 10 // 100ms chunks
)

var pcmFormat = audio.Format{
	Codec:      audio.CodecPCM,
	SampleRate: sampleRate,
	Channels:   numChannels,
	BitDepth:   16,
}

// PortAudioSource implements speech.AudioSource backed by a PortAudio input stream.
type PortAudioSource struct {
	paStream *portaudio.Stream
	buf      []int16
	pipe     *audio.Pipe[audio.Frame]
	done     chan struct{}
}

var _ speech.AudioSource = (*PortAudioSource)(nil)

func NewPortAudioSource() (*PortAudioSource, error) {
	s := &PortAudioSource{
		buf:  make([]int16, framesPerBuffer*numChannels),
		pipe: audio.NewPipe[audio.Frame](30),
		done: make(chan struct{}),
	}
	var err error
	s.paStream, err = portaudio.OpenDefaultStream(numChannels, 0, float64(sampleRate), framesPerBuffer, &s.buf)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Start begins reading audio from the microphone.
func (s *PortAudioSource) Start(ctx context.Context) error {
	if err := s.paStream.Start(); err != nil {
		return err
	}
	go func() {
		defer close(s.done)
		for {
			if err := s.paStream.Read(); err != nil {
				s.pipe.Close()
				return
			}
			data := make([]byte, len(s.buf)*2)
			for i, sample := range s.buf {
				binary.LittleEndian.PutUint16(data[i*2:], uint16(sample))
			}
			f := audio.Frame{Data: data, Format: pcmFormat}
			if !s.pipe.Send(f) {
				return
			}
		}
	}()
	go func() {
		<-ctx.Done()
		s.pipe.Interrupt()
	}()
	return nil
}

// Stream returns the audio frame stream (used by speech.Session).
func (s *PortAudioSource) Stream() audio.Stream[audio.Frame] { return s.pipe }

// Close stops the PortAudio input stream.
func (s *PortAudioSource) Close() error {
	s.pipe.Interrupt()
	<-s.done
	s.paStream.Stop()
	return s.paStream.Close()
}

// PortAudioSink implements speech.AudioSink for MP3 TTS chunks via PortAudio output.
type PortAudioSink struct {
	ref *audio.Pipe[audio.Frame]
}

var (
	_ speech.AudioSink                 = (*PortAudioSink)(nil)
	_ speech.PlaybackReferenceProvider = (*PortAudioSink)(nil)
)

func NewPortAudioSink() *PortAudioSink {
	return &PortAudioSink{ref: audio.NewPipe[audio.Frame](64)}
}

func (s *PortAudioSink) PlaybackReference() audio.Stream[audio.Frame] { return s.ref }

// Play starts playback asynchronously and returns when drained or aborted.
func (s *PortAudioSink) Play(stream audio.Stream[tts.Utterance]) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)

		var (
			paStream *portaudio.Stream
			buf      []int16
			rate     int
			bufLen   int
		)
		const ch = 2

		cleanup := func(abort bool) {
			if paStream != nil {
				if abort {
					paStream.Abort()
				} else {
					paStream.Stop()
				}
				paStream.Close()
				paStream = nil
			}
		}

		defer cleanup(false)

		for {
			u, err := stream.Read()
			if err == io.EOF {
				return
			}
			if err != nil {
				cleanup(true)
				return
			}
			playData(u.Data, ch, &paStream, &buf, &rate, &bufLen, s.ref)
		}
	}()
	return done
}

func playData(data []byte, ch int, paStream **portaudio.Stream, buf *[]int16, rate *int, bufLen *int, ref *audio.Pipe[audio.Frame]) {
	dec, err := mp3.NewDecoder(bytes.NewReader(data))
	if err != nil {
		fmt.Printf("  mp3 decode: %v\n", err)
		return
	}
	sr := dec.SampleRate()

	pcm, err := io.ReadAll(dec)
	if err != nil || len(pcm) == 0 {
		return
	}

	samples := bytesToInt16(pcm)

	if *paStream == nil || sr != *rate {
		if *paStream != nil {
			(*paStream).Stop()
			(*paStream).Close()
		}
		*rate = sr
		*bufLen = sr / 10 * ch
		*buf = make([]int16, *bufLen)
		stream, err := portaudio.OpenDefaultStream(0, ch, float64(sr), sr/10, buf)
		if err != nil {
			fmt.Printf("  open speaker: %v\n", err)
			return
		}
		if err := stream.Start(); err != nil {
			stream.Close()
			return
		}
		*paStream = stream
	}

	for off := 0; off < len(samples); off += *bufLen {
		end := off + *bufLen
		var chunk []int16
		if end > len(samples) {
			clear(*buf)
			copy(*buf, samples[off:])
			chunk = samples[off:]
		} else {
			copy(*buf, samples[off:end])
			chunk = samples[off:end]
		}
		if ref != nil && len(chunk) > 0 {
			b := make([]byte, len(chunk)*2)
			for i, sample := range chunk {
				binary.LittleEndian.PutUint16(b[i*2:], uint16(sample))
			}
			ref.TrySend(audio.Frame{
				Data: b,
				Format: audio.Format{
					Codec:      audio.CodecPCM,
					SampleRate: sr,
					Channels:   ch,
					BitDepth:   16,
				},
			})
		}
		if (*paStream).Write() != nil {
			return
		}
	}
}

func bytesToInt16(data []byte) []int16 {
	n := len(data) / 2
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return out
}
