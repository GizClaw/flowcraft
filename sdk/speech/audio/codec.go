package audio

import "time"

// Codec identifies an audio encoding format.
type Codec int

const (
	CodecPCM Codec = iota
	CodecMP3
	CodecOpus
	CodecWAV
)

// Format describes the encoding parameters of an audio stream.
type Format struct {
	Codec      Codec
	SampleRate int
	Channels   int
	BitDepth   int
}

// Frame is a self-describing chunk of audio data.
type Frame struct {
	Data        []byte
	Format      Format
	Timestamp   time.Duration
	Duration    time.Duration
	Sequence    int64
	CaptureTime time.Time
	SourceID    string
}
