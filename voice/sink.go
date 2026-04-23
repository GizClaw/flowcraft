package speech

import (
	"github.com/GizClaw/flowcraft/voice/audio"
	"github.com/GizClaw/flowcraft/voice/tts"
)

// AudioSink is an abstraction for audio output (speaker, WebSocket, file, etc.).
type AudioSink interface {
	// Play starts playing utterances from the stream asynchronously.
	// It returns a channel that is closed when playback finishes (drained or aborted).
	//
	// Stream termination semantics:
	//   - io.EOF means the turn ended normally — drain the hardware buffer before signalling done.
	//   - Any other error means interruption — discard buffered audio and signal done immediately.
	Play(stream audio.Stream[tts.Utterance]) <-chan struct{}
}

// PlaybackReferenceProvider is an optional interface for sinks that can expose
// the audio they are currently playing as a reference stream for echo
// suppression or AEC.
type PlaybackReferenceProvider interface {
	PlaybackReference() audio.Stream[audio.Frame]
}
