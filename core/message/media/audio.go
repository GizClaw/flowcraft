package media

import (
	"bytes"
	"encoding/binary"
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

// AudioDurationMillis derives the duration of an encoded audio payload in
// milliseconds when it can be computed without a full decoder. Raw PCM
// families are exact from the byte count; MPEG audio (mp3) is estimated
// from its frame headers. ok reports false for encodings that require a
// real decoder (opus, aac, flac) or for malformed payloads, so callers can
// distinguish "unknown" from a real zero.
func AudioDurationMillis(data []byte, format AudioFormat) (int64, bool) {
	if len(data) == 0 {
		return 0, false
	}
	switch format.Encoding {
	case AudioEncodingPCM16, AudioEncodingPCM24, AudioEncodingFloat32:
		if format.SampleRateHz <= 0 || format.Channels <= 0 {
			return 0, false
		}
		bytesPerSample := map[AudioEncoding]int64{
			AudioEncodingPCM16:   2,
			AudioEncodingPCM24:   3,
			AudioEncodingFloat32: 4,
		}[format.Encoding]
		frameBytes := bytesPerSample * int64(format.Channels)
		return int64(len(data)) / frameBytes * 1000 / int64(format.SampleRateHz), true
	case AudioEncodingMP3:
		return mpegAudioDurationMillis(data)
	default:
		return 0, false
	}
}

// mpegAudioDurationMillis walks MPEG audio frame headers and sums the
// samples they declare. Layer III MPEG-1 frames carry 1152 samples and
// MPEG-2/2.5 frames 576, so the estimate stays accurate for CBR and VBR
// alike as long as every frame is present. It skips a leading ID3v2 tag and
// stops at the first boundary that does not parse as another frame.
func mpegAudioDurationMillis(data []byte) (int64, bool) {
	pos := skipID3v2(data)
	var totalSamples, sampleRate int
	for pos+4 <= len(data) {
		if data[pos] != 0xff || data[pos+1]&0xe0 != 0xe0 {
			pos++
			continue
		}
		header := binary.BigEndian.Uint32(data[pos : pos+4])
		bitrate, rate, padding, samples, ok := mpegFrameParams(header)
		if !ok || bitrate == 0 || rate == 0 {
			pos++
			continue
		}
		frameLen := mpegFrameLength(header, bitrate, rate, padding, samples)
		if frameLen <= 0 || pos+frameLen > len(data) {
			break
		}
		if sampleRate == 0 {
			sampleRate = rate
		} else if rate != sampleRate {
			// Mixed sample rates cannot be summed meaningfully.
			return 0, false
		}
		totalSamples += samples
		pos += frameLen
	}
	if sampleRate == 0 || totalSamples == 0 {
		return 0, false
	}
	return int64(totalSamples) * 1000 / int64(sampleRate), true
}

// skipID3v2 returns the offset just past a leading ID3v2 tag, or zero when
// the payload does not begin with one.
func skipID3v2(data []byte) int {
	if len(data) <= 10 || !bytes.Equal(data[:3], []byte("ID3")) {
		return 0
	}
	size := int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
	total := 10 + size
	if data[5]&0x10 != 0 {
		total += 10 // footer flag mirrors the header.
	}
	if total <= len(data) {
		return total
	}
	return 0
}

// mpegFrameParams decodes version, layer, bitrate (bits per second), sample
// rate, padding, and samples per frame from a 4-byte MPEG audio header.
func mpegFrameParams(header uint32) (
	bitrate, rate, padding, samples int,
	ok bool,
) {
	version := (header >> 19) & 0x3
	layer := (header >> 17) & 0x3
	if version == 0x1 || layer == 0x0 {
		return 0, 0, 0, 0, false
	}
	bitrateIndex := int((header >> 12) & 0xf)
	rateIndex := int((header >> 10) & 0x3)
	if bitrateIndex == 0xf || bitrateIndex == 0 || rateIndex == 0x3 {
		return 0, 0, 0, 0, false
	}
	padding = int((header >> 9) & 0x1)
	bitrate = mpegBitrate(version, layer, bitrateIndex)
	rate = mpegSampleRate(version, rateIndex)
	samples = mpegSamplesPerFrame(version, layer)
	if bitrate == 0 || rate == 0 || samples == 0 {
		return 0, 0, 0, 0, false
	}
	return bitrate, rate, padding, samples, true
}

func mpegFrameLength(
	header uint32,
	bitrate, rate, padding, samples int,
) int {
	if (header>>17)&0x3 == 0x3 { // Layer I
		return (12*bitrate/rate + padding) * 4
	}
	return samples*bitrate/(8*rate) + padding
}

func mpegBitrate(version, layer uint32, index int) int {
	rates := [][]int{
		// MPEG-1
		{32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, 0}, // Layer I
		{32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 0},    // Layer II
		{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0},     // Layer III
		{32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, 0},    // Layer I
		{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},         // Layer II
		{8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, 0},         // Layer III
	}
	table := 0
	if version != 0x3 {
		table = 3
	}
	table += 3 - int(layer)
	return rates[table][index-1] * 1000
}

func mpegSampleRate(version uint32, index int) int {
	rates := [][]int{
		{44100, 48000, 32000}, // MPEG-1
		{22050, 24000, 16000}, // MPEG-2
		{11025, 12000, 8000},  // MPEG-2.5
	}
	switch version {
	case 0x3:
		return rates[0][index]
	case 0x2:
		return rates[1][index]
	case 0x0:
		return rates[2][index]
	default:
		return 0
	}
}

func mpegSamplesPerFrame(version, layer uint32) int {
	switch layer {
	case 0x3: // Layer I
		return 384
	case 0x2: // Layer II
		return 1152
	default: // Layer III
		if version == 0x3 {
			return 1152
		}
		return 576
	}
}
