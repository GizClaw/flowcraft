package webrtc

import (
	"sync"
	"time"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/pion/webrtc/v4"
)

// SourceConfig describes the PCM format produced by the AudioDecoder.
// Used to populate Frame.Format metadata accurately.
type SourceConfig struct {
	SampleRate int // e.g. 48000; defaults to 48000 if zero
	Channels   int // e.g. 1; defaults to 1 if zero
}

func (c SourceConfig) sampleRate() int {
	if c.SampleRate > 0 {
		return c.SampleRate
	}
	return 48000
}

func (c SourceConfig) channels() int {
	if c.Channels > 0 {
		return c.Channels
	}
	return 1
}

// Source implements speech.AudioSource by reading RTP packets from a remote
// WebRTC audio track, decoding the payload via the injected AudioDecoder,
// and emitting audio.Frame values through a Pipe.
type Source struct {
	pipe     *audio.Pipe[audio.Frame]
	decoder  AudioDecoder
	config   SourceConfig
	initOnce sync.Once
}

// Stream returns the audio frame stream. Implements speech.AudioSource.
func (s *Source) Stream() audio.Stream[audio.Frame] {
	return s.pipe
}

func newSource(decoder AudioDecoder, cfg SourceConfig) *Source {
	return &Source{
		pipe:    audio.NewPipe[audio.Frame](64),
		decoder: decoder,
		config:  cfg,
	}
}

// readLoop runs in a goroutine, reading RTP packets from the remote track,
// extracting the payload, decoding it, and sending PCM frames into the pipe.
// Only the first invocation runs; subsequent calls are no-ops (safe under renegotiation).
func (s *Source) readLoop(track *webrtc.TrackRemote) {
	started := false
	s.initOnce.Do(func() { started = true })
	if !started {
		return
	}

	defer s.pipe.Close()

	var seq int64
	format := audio.Format{
		Codec:      audio.CodecPCM,
		SampleRate: s.config.sampleRate(),
		Channels:   s.config.channels(),
		BitDepth:   16,
	}

	for {
		pkt, _, err := track.ReadRTP()
		if err != nil {
			return
		}
		if len(pkt.Payload) == 0 {
			continue
		}

		pcm, decErr := s.decoder.Decode(pkt.Payload)
		if decErr != nil || len(pcm) == 0 {
			continue
		}

		frame := audio.Frame{
			Data:        pcm,
			Format:      format,
			Sequence:    seq,
			CaptureTime: time.Now(),
		}
		seq++

		if !s.pipe.Send(frame) {
			return
		}
	}
}
