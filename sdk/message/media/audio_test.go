package media

import "testing"

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
