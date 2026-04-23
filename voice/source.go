package voice

import "github.com/GizClaw/flowcraft/voice/audio"

// AudioSource is an abstraction for audio input (microphone, WebSocket, file, etc.).
//
// Implementation contract: when the context passed to Start (or the source's
// owning context) is cancelled, Stream().Read() must return an error (typically
// via Pipe.Interrupt). This is required for Session.Run to exit promptly on
// Ctrl+C. Implementations that do not honour this contract will cause
// Session.Run to block indefinitely on src.Read().
type AudioSource interface {
	Stream() audio.Stream[audio.Frame]
}
