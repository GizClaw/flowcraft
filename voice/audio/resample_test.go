package audio_test

import (
	"encoding/binary"
	"io"
	"math"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/voice/audio"
)

// --- helpers ---

func makePCM16Sine(freq float64, sampleRate, numSamples, channels int) []byte {
	data := make([]byte, numSamples*channels*2)
	for i := range numSamples {
		val := int16(16000 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		for ch := range channels {
			binary.LittleEndian.PutUint16(data[(i*channels+ch)*2:], uint16(val))
		}
	}
	return data
}

func decodePCM16Samples(data []byte) []int16 {
	n := len(data) / 2
	out := make([]int16, n)
	for i := range n {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*2:]))
	}
	return out
}

// --- ResamplePCM16 unit tests ---

func TestResamplePCM16_SameRate(t *testing.T) {
	data := makePCM16Sine(440, 16000, 1600, 1)
	out := audio.ResamplePCM16(data, 16000, 16000, 1)
	if &out[0] != &data[0] {
		t.Error("same-rate should return original slice without copying")
	}
}

func TestResamplePCM16_DownsampleLength(t *testing.T) {
	data := makePCM16Sine(440, 24000, 2400, 1)
	out := audio.ResamplePCM16(data, 24000, 16000, 1)
	wantFrames := 2400 * 16000 / 24000 // 1600
	if len(out) != wantFrames*2 {
		t.Errorf("len = %d, want %d bytes", len(out), wantFrames*2)
	}
}

func TestResamplePCM16_UpsampleLength(t *testing.T) {
	data := makePCM16Sine(440, 16000, 1600, 1)
	out := audio.ResamplePCM16(data, 16000, 48000, 1)
	wantFrames := 1600 * 48000 / 16000 // 4800
	if len(out) != wantFrames*2 {
		t.Errorf("len = %d, want %d bytes", len(out), wantFrames*2)
	}
}

func TestResamplePCM16_Stereo(t *testing.T) {
	data := makePCM16Sine(440, 24000, 2400, 2)
	out := audio.ResamplePCM16(data, 24000, 16000, 2)
	wantFrames := 2400 * 16000 / 24000
	if len(out) != wantFrames*2*2 {
		t.Errorf("len = %d, want %d bytes (stereo)", len(out), wantFrames*2*2)
	}
}

func TestResamplePCM16_PreservesFrequency(t *testing.T) {
	const (
		srcRate = 48000
		dstRate = 16000
		freq    = 440.0
	)
	srcSamples := srcRate // 1 second
	data := makePCM16Sine(freq, srcRate, srcSamples, 1)
	out := audio.ResamplePCM16(data, srcRate, dstRate, 1)

	samples := decodePCM16Samples(out)
	if len(samples) < dstRate/2 {
		t.Fatalf("not enough output samples: %d", len(samples))
	}

	// Count zero crossings in the output to verify frequency is preserved.
	// Expected crossings per second ≈ 2 * freq.
	var crossings int
	for i := 1; i < len(samples); i++ {
		if (samples[i] >= 0) != (samples[i-1] >= 0) {
			crossings++
		}
	}
	detectedFreq := float64(crossings) / 2.0 * float64(dstRate) / float64(len(samples))
	tolerance := freq * 0.05 // 5% tolerance
	if math.Abs(detectedFreq-freq) > tolerance {
		t.Errorf("detected freq %.1f Hz, want ~%.1f Hz (tolerance ±%.1f)", detectedFreq, freq, tolerance)
	}
}

func TestResamplePCM16_EmptyInput(t *testing.T) {
	out := audio.ResamplePCM16(nil, 24000, 16000, 1)
	if out != nil {
		t.Errorf("expected nil for nil input, got %d bytes", len(out))
	}
}

func TestResamplePCM16_SingleSample(t *testing.T) {
	data := make([]byte, 2)
	binary.LittleEndian.PutUint16(data, uint16(int16(1000)))
	out := audio.ResamplePCM16(data, 24000, 16000, 1)
	// Single source frame → at least 1 output frame (ratio < 1 truncates to 0,
	// but our implementation returns nil for 0 output frames).
	// 1 * 16000 / 24000 = 0 → nil
	if out != nil {
		t.Errorf("single sample downsample: expected nil, got %d bytes", len(out))
	}
}

func TestResamplePCM16_InvalidRates(t *testing.T) {
	data := makePCM16Sine(440, 16000, 160, 1)
	cases := []struct {
		name     string
		from, to int
		ch       int
	}{
		{"zero from", 0, 16000, 1},
		{"zero to", 16000, 0, 1},
		{"negative from", -1, 16000, 1},
		{"zero channels", 16000, 8000, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := audio.ResamplePCM16(data, tc.from, tc.to, tc.ch)
			if &out[0] != &data[0] {
				t.Error("invalid params should return original slice")
			}
		})
	}
}

func TestResamplePCM16_ClampOverflow(t *testing.T) {
	data := make([]byte, 4)
	binary.LittleEndian.PutUint16(data[0:], uint16(32767))
	binary.LittleEndian.PutUint16(data[2:], uint16(32768)) // -32768 in two's complement
	out := audio.ResamplePCM16(data, 8000, 48000, 1)
	samples := decodePCM16Samples(out)
	if len(samples) == 0 {
		t.Fatal("expected resampled PCM samples")
	}
}

// --- ResampleStream unit tests ---

func TestResampleStream_Passthrough(t *testing.T) {
	pipe := audio.NewPipe[audio.Frame](4)
	f := audio.Frame{
		Data:   makePCM16Sine(440, 16000, 160, 1),
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 16000, Channels: 1, BitDepth: 16},
	}
	go func() {
		pipe.Send(f)
		pipe.Close()
	}()

	stream := audio.ResampleStream(pipe, 16000)
	out, err := stream.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != len(f.Data) {
		t.Errorf("passthrough: len changed from %d to %d", len(f.Data), len(out.Data))
	}
	if out.Format.SampleRate != 16000 {
		t.Errorf("sample rate = %d, want 16000", out.Format.SampleRate)
	}
}

func TestResampleStream_Downsample(t *testing.T) {
	pipe := audio.NewPipe[audio.Frame](4)
	f := audio.Frame{
		Data:        makePCM16Sine(440, 24000, 2400, 1),
		Format:      audio.Format{Codec: audio.CodecPCM, SampleRate: 24000, Channels: 1, BitDepth: 16},
		Timestamp:   250 * time.Millisecond,
		Duration:    100 * time.Millisecond,
		Sequence:    7,
		CaptureTime: time.Unix(123, 0),
		SourceID:    "mic-a",
	}
	go func() {
		pipe.Send(f)
		pipe.Close()
	}()

	stream := audio.ResampleStream(pipe, 16000)
	out, err := stream.Read()
	if err != nil {
		t.Fatal(err)
	}
	wantBytes := (2400 * 16000 / 24000) * 2
	if len(out.Data) != wantBytes {
		t.Errorf("len = %d, want %d", len(out.Data), wantBytes)
	}
	if out.Format.SampleRate != 16000 {
		t.Errorf("sample rate = %d, want 16000", out.Format.SampleRate)
	}
	if out.Timestamp != f.Timestamp || out.Duration != f.Duration || out.Sequence != f.Sequence ||
		!out.CaptureTime.Equal(f.CaptureTime) || out.SourceID != f.SourceID {
		t.Errorf("metadata changed after resample: got %+v want %+v", out, f)
	}
}

func TestResampleStream_NonPCMPassthrough(t *testing.T) {
	pipe := audio.NewPipe[audio.Frame](4)
	f := audio.Frame{
		Data:   []byte{0xff, 0xfb, 0x90, 0x00}, // fake MP3 header
		Format: audio.Format{Codec: audio.CodecMP3, SampleRate: 44100},
	}
	go func() {
		pipe.Send(f)
		pipe.Close()
	}()

	stream := audio.ResampleStream(pipe, 16000)
	out, err := stream.Read()
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != len(f.Data) {
		t.Error("non-PCM frame should pass through unchanged")
	}
	if out.Format.SampleRate != 44100 {
		t.Errorf("non-PCM sample rate should be unchanged, got %d", out.Format.SampleRate)
	}
}

func TestResampleStream_MultipleFrames(t *testing.T) {
	pipe := audio.NewPipe[audio.Frame](8)
	const numFrames = 5
	go func() {
		for range numFrames {
			pipe.Send(audio.Frame{
				Data:   makePCM16Sine(440, 48000, 4800, 1),
				Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 48000, Channels: 1, BitDepth: 16},
			})
		}
		pipe.Close()
	}()

	stream := audio.ResampleStream(pipe, 16000)
	var count int
	for {
		f, err := stream.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		count++
		wantBytes := (4800 * 16000 / 48000) * 2
		if len(f.Data) != wantBytes {
			t.Errorf("frame %d: len = %d, want %d", count, len(f.Data), wantBytes)
		}
	}
	if count != numFrames {
		t.Errorf("read %d frames, want %d", count, numFrames)
	}
}

func TestResampleStream_ZeroTarget(t *testing.T) {
	pipe := audio.NewPipe[audio.Frame](4)
	stream := audio.ResampleStream(pipe, 0)
	if stream != audio.Stream[audio.Frame](pipe) {
		t.Error("zero target rate should return input stream directly")
	}
	pipe.Close()
}

// --- Benchmarks ---

func BenchmarkResamplePCM16_24kTo16k_100ms(b *testing.B) {
	data := makePCM16Sine(440, 24000, 2400, 1) // 100ms at 24kHz
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		audio.ResamplePCM16(data, 24000, 16000, 1)
	}
}

func BenchmarkResamplePCM16_48kTo16k_100ms(b *testing.B) {
	data := makePCM16Sine(440, 48000, 4800, 1) // 100ms at 48kHz
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		audio.ResamplePCM16(data, 48000, 16000, 1)
	}
}

func BenchmarkResamplePCM16_16kTo48k_100ms(b *testing.B) {
	data := makePCM16Sine(440, 16000, 1600, 1) // 100ms at 16kHz
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		audio.ResamplePCM16(data, 16000, 48000, 1)
	}
}

func BenchmarkResamplePCM16_24kTo16k_1s(b *testing.B) {
	data := makePCM16Sine(440, 24000, 24000, 1) // 1s at 24kHz
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		audio.ResamplePCM16(data, 24000, 16000, 1)
	}
}

func BenchmarkResamplePCM16_SameRate(b *testing.B) {
	data := makePCM16Sine(440, 16000, 1600, 1)
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		audio.ResamplePCM16(data, 16000, 16000, 1)
	}
}

func BenchmarkResamplePCM16_Stereo_24kTo16k(b *testing.B) {
	data := makePCM16Sine(440, 24000, 2400, 2) // 100ms stereo
	b.SetBytes(int64(len(data)))
	b.ResetTimer()
	for range b.N {
		audio.ResamplePCM16(data, 24000, 16000, 2)
	}
}

func BenchmarkResampleStream_24kTo16k(b *testing.B) {
	const frames = 100 // 100 * 100ms = 10s of audio
	frameData := makePCM16Sine(440, 24000, 2400, 1)
	fmt := audio.Format{Codec: audio.CodecPCM, SampleRate: 24000, Channels: 1, BitDepth: 16}
	b.SetBytes(int64(len(frameData)) * frames)
	b.ResetTimer()

	for range b.N {
		pipe := audio.NewPipe[audio.Frame](16)
		go func() {
			for range frames {
				pipe.Send(audio.Frame{Data: frameData, Format: fmt})
			}
			pipe.Close()
		}()
		stream := audio.ResampleStream(pipe, 16000)
		for {
			_, err := stream.Read()
			if err != nil {
				break
			}
		}
	}
}
