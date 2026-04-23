package speech

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/speech/audio"
	"github.com/GizClaw/flowcraft/sdk/speech/stt"
)

func makePCM16SineResampleTest(freq float64, sampleRate, numSamples, channels int) []byte {
	data := make([]byte, numSamples*channels*2)
	for i := range numSamples {
		val := int16(16000 * math.Sin(2*math.Pi*freq*float64(i)/float64(sampleRate)))
		for ch := range channels {
			binary.LittleEndian.PutUint16(data[(i*channels+ch)*2:], uint16(val))
		}
	}
	return data
}

// --- Pipeline resample integration ---

func TestPipeline_ResampleFrame(t *testing.T) {
	p := &Pipeline{
		sttOpts: []stt.STTOption{stt.WithTargetSampleRate(16000)},
	}
	f := audio.Frame{
		Data:   makePCM16SineResampleTest(440, 24000, 2400, 1),
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 24000, Channels: 1, BitDepth: 16},
	}
	out := p.resampleFrame(f)
	if out.Format.SampleRate != 16000 {
		t.Errorf("sample rate = %d, want 16000", out.Format.SampleRate)
	}
	wantBytes := (2400 * 16000 / 24000) * 2
	if len(out.Data) != wantBytes {
		t.Errorf("len = %d, want %d", len(out.Data), wantBytes)
	}
}

func TestPipeline_ResampleFrame_NoTarget(t *testing.T) {
	p := &Pipeline{}
	data := makePCM16SineResampleTest(440, 24000, 2400, 1)
	f := audio.Frame{
		Data:   data,
		Format: audio.Format{Codec: audio.CodecPCM, SampleRate: 24000, Channels: 1, BitDepth: 16},
	}
	out := p.resampleFrame(f)
	if len(out.Data) != len(data) {
		t.Error("no target rate: data should be unchanged")
	}
}
