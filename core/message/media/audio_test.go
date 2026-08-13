package media

import (
	"encoding/binary"
	"testing"
)

func TestAudioFormatAndChunksValidate(t *testing.T) {
	format := AudioFormat{
		Encoding:     AudioEncodingPCM16,
		SampleRateHz: 24_000,
		Channels:     1,
	}
	if err := format.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := (AudioFormat{Encoding: AudioEncodingPCM16}).Validate(); err == nil {
		t.Fatal("PCM format accepted without sample rate and channels")
	}
	chunk := AudioChunk{Data: []byte("pcm"), Sequence: 1}
	clone := chunk.Clone()
	clone.Data[0] = 'X'
	if string(chunk.Data) != "pcm" {
		t.Fatal("AudioChunk.Clone shared byte storage")
	}
	if err := chunk.Validate(); err != nil {
		t.Fatalf("chunk Validate: %v", err)
	}
}

func TestAudioDurationMillis(t *testing.T) {
	t.Run("raw PCM is exact", func(t *testing.T) {
		// 1 second of 16-bit stereo at 44.1 kHz = 176400 bytes.
		format := AudioFormat{
			Encoding:     AudioEncodingPCM16,
			SampleRateHz: 44100,
			Channels:     2,
		}
		millis, ok := AudioDurationMillis(make([]byte, 176400), format)
		if !ok || millis != 1000 {
			t.Fatalf("AudioDurationMillis = (%d, %v), want (1000, true)", millis, ok)
		}
	})

	t.Run("mp3 frame headers", func(t *testing.T) {
		// 10 MPEG-1 Layer III frames at 128 kbps / 44.1 kHz, no padding.
		// Each frame holds 1152 samples and spans 417 bytes.
		const frameLen = 417
		payload := make([]byte, 10*frameLen)
		for i := 0; i < 10; i++ {
			header := uint32(0xfffb_9000) // sync, MPEG1, Layer III, 128kbps, 44.1kHz
			binary.BigEndian.PutUint32(payload[i*frameLen:], header)
		}
		millis, ok := AudioDurationMillis(payload, AudioFormat{Encoding: AudioEncodingMP3})
		if !ok {
			t.Fatal("AudioDurationMillis(mp3) = not ok")
		}
		want := int64(10 * 1152 * 1000 / 44100)
		if millis != want {
			t.Fatalf("AudioDurationMillis(mp3) = %d, want %d", millis, want)
		}
	})

	t.Run("mp3 skips leading ID3v2", func(t *testing.T) {
		const frameLen = 417
		payload := make([]byte, 0, 10+8+2*frameLen)
		tag := make([]byte, 10+8)
		copy(tag, "ID3")
		tag[3], tag[4], tag[5] = 4, 0, 0
		tag[6], tag[7], tag[8], tag[9] = 0, 0, 0, 8 // synchsafe size 8
		payload = append(payload, tag...)
		frames := make([]byte, 2*frameLen)
		for i := 0; i < 2; i++ {
			binary.BigEndian.PutUint32(frames[i*frameLen:], 0xfffb_9000)
		}
		payload = append(payload, frames...)
		millis, ok := AudioDurationMillis(payload, AudioFormat{Encoding: AudioEncodingMP3})
		if !ok {
			t.Fatal("AudioDurationMillis(mp3 with ID3v2) = not ok")
		}
		want := int64(2 * 1152 * 1000 / 44100)
		if millis != want {
			t.Fatalf("AudioDurationMillis(mp3 with ID3v2) = %d, want %d", millis, want)
		}
	})

	t.Run("unsupported encoding", func(t *testing.T) {
		if _, ok := AudioDurationMillis(
			[]byte("opus"),
			AudioFormat{Encoding: AudioEncodingOpus},
		); ok {
			t.Fatal("AudioDurationMillis(opus) = ok, want not ok")
		}
	})
}
