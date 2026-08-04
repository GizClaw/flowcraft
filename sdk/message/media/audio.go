package media

import (
	"bytes"
	"fmt"
)

type AudioEncoding string

const (
	AudioEncodingPCM16   AudioEncoding = "pcm16"
	AudioEncodingPCM24   AudioEncoding = "pcm24"
	AudioEncodingFloat32 AudioEncoding = "float32"
	AudioEncodingMP3     AudioEncoding = "mp3"
	AudioEncodingOpus    AudioEncoding = "opus"
	AudioEncodingAAC     AudioEncoding = "aac"
	AudioEncodingFLAC    AudioEncoding = "flac"
)

func (e AudioEncoding) MediaType() string {
	switch e {
	case AudioEncodingPCM16, AudioEncodingPCM24, AudioEncodingFloat32:
		return "audio/pcm"
	case AudioEncodingMP3:
		return "audio/mpeg"
	case AudioEncodingOpus:
		return "audio/opus"
	case AudioEncodingAAC:
		return "audio/aac"
	case AudioEncodingFLAC:
		return "audio/flac"
	default:
		return ""
	}
}

type AudioFormat struct {
	Encoding     AudioEncoding `json:"encoding"`
	SampleRateHz int           `json:"sample_rate_hz,omitempty"`
	Channels     int           `json:"channels,omitempty"`
}

func (f AudioFormat) Validate() error {
	switch f.Encoding {
	case AudioEncodingPCM16, AudioEncodingPCM24, AudioEncodingFloat32:
		if f.SampleRateHz <= 0 || f.Channels <= 0 {
			return fmt.Errorf("raw audio format requires sample rate and channels")
		}
	case AudioEncodingMP3, AudioEncodingOpus, AudioEncodingAAC, AudioEncodingFLAC:
		if f.SampleRateHz < 0 || f.Channels < 0 {
			return fmt.Errorf("audio sample rate and channels must not be negative")
		}
	default:
		return fmt.Errorf("unknown audio encoding %q", f.Encoding)
	}
	return nil
}

type VoiceSpec struct {
	ID       string `json:"id"`
	Language string `json:"language,omitempty"`
}

func (v VoiceSpec) Validate() error {
	if v.ID == "" {
		return fmt.Errorf("voice ID is required")
	}
	return nil
}

type AudioChunk struct {
	Data     []byte `json:"data"`
	Sequence uint64 `json:"sequence,omitempty"`
}

func (c AudioChunk) Clone() AudioChunk {
	c.Data = bytes.Clone(c.Data)
	return c
}

func (c AudioChunk) Validate() error {
	if len(c.Data) == 0 {
		return fmt.Errorf("audio chunk data is required")
	}
	return nil
}
