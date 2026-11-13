package minimax

import (
	stdjson "encoding/json"
	"fmt"
	"sort"
	"testing"
)

// canonicalJSON renders a request body as sorted-key JSON, so a golden asserts what
// the wire says rather than the order a struct happens to emit it in.
func canonicalJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	var generic any
	if err := stdjson.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	normalized, err := stdjson.Marshal(sortKeys(generic))
	if err != nil {
		t.Fatalf("re-encode body: %v", err)
	}
	return string(normalized)
}

// sortKeys walks a decoded body so map keys come out sorted at every depth.
func sortKeys(v any) any {
	switch value := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(value))
		for key, item := range value {
			out[key] = sortKeys(item)
		}
		keys := make([]string, 0, len(out))
		for key := range out {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		ordered := make([]string, 0, len(keys))
		for _, key := range keys {
			encoded, _ := stdjson.Marshal(out[key])
			ordered = append(ordered, fmt.Sprintf("%q:%s", key, encoded))
		}
		return rawObject(ordered)
	case []any:
		out := make([]any, len(value))
		for i, item := range value {
			out[i] = sortKeys(item)
		}
		return out
	default:
		return v
	}
}

// rawObject marshals as a verbatim JSON object, which preserves the sort while
// keeping the comparison byte-exact.
type rawObject []string

func (o rawObject) MarshalJSON() ([]byte, error) {
	out := []byte("{")
	for i, field := range o {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, field...)
	}
	return append(out, '}'), nil
}

// TestSpeechWireBody pins the t2a_v2 request body: the fields the platform requires
// are always present, and an unset audio knob disappears rather than sending a zero.
func TestSpeechWireBody(t *testing.T) {
	cases := []struct {
		name string
		wire ttsWire
		want string
	}{
		{
			name: "minimal",
			wire: ttsWire{model: "speech-2.6-hd", text: "hi", voice: "v"},
			want: `{"audio_setting":{},"language_boost":"auto","model":"speech-2.6-hd","output_format":"hex","stream":false,"text":"hi","voice_setting":{"voice_id":"v"}}`,
		},
		{
			name: "full",
			wire: ttsWire{model: "speech-2.6-hd", text: "hi", voice: "v", stream: true,
				language: "Chinese", speed: floatPtr(1.25), format: "pcm", sample: 24000, channels: 1},
			want: `{"audio_setting":{"channel":1,"format":"pcm","sample_rate":24000},"language_boost":"Chinese","model":"speech-2.6-hd","output_format":"hex","stream":true,"text":"hi","voice_setting":{"speed":1.25,"voice_id":"v"}}`,
		},
		{
			name: "mp3",
			wire: ttsWire{model: "m", text: "t", voice: "v", format: "mp3"},
			want: `{"audio_setting":{"format":"mp3"},"language_boost":"auto","model":"m","output_format":"hex","stream":false,"text":"t","voice_setting":{"voice_id":"v"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalJSON(t, ttsRequest(tc.wire)); got != tc.want {
				t.Fatalf("body drifted:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// TestVideoTaskWireBody pins the H3 task body across the input shapes: text only,
// first/last frames, multimodal references, and the optional switches.
func TestVideoTaskWireBody(t *testing.T) {
	falseValue := false
	cases := []struct {
		name string
		wire videoWire
		want string
	}{
		{
			name: "text-only",
			wire: videoWire{model: "MiniMax-H3", prompt: "a lake", duration: intValue(6),
				resolution: "768P", ratio: "16:9"},
			want: `{"content":[{"text":"a lake","type":"text"}],"duration":6,"model":"MiniMax-H3","ratio":"16:9","resolution":"768P"}`,
		},
		{
			name: "watermark-callback",
			wire: videoWire{model: "MiniMax-H3", prompt: "a lake", duration: intValue(10),
				resolution: "2K", watermark: &falseValue, callback: "https://example.com/cb"},
			want: `{"aigc_watermark":false,"callback_url":"https://example.com/cb","content":[{"text":"a lake","type":"text"}],"duration":10,"model":"MiniMax-H3","resolution":"2K"}`,
		},
		{
			name: "frames",
			wire: videoWire{model: "MiniMax-H3", prompt: "morph", duration: intValue(4),
				resolution: "768P", firstFrame: "https://a/1.png", lastFrame: "https://a/2.png"},
			want: `{"content":[{"text":"morph","type":"text"},{"image_url":{"url":"https://a/1.png"},"role":"first_frame","type":"image_url"},{"image_url":{"url":"https://a/2.png"},"role":"last_frame","type":"image_url"}],"duration":4,"model":"MiniMax-H3","resolution":"768P"}`,
		},
		{
			name: "references",
			wire: videoWire{model: "MiniMax-H3", prompt: "restyle", duration: intValue(8),
				resolution: "2K", ratio: "adaptive",
				referenceImages: []string{"https://a/1.png", "https://a/2.png"},
				referenceVideos: []string{"https://a/v.mp4"},
				referenceAudios: []string{"https://a/a.mp3"}},
			want: `{"content":[{"text":"restyle","type":"text"},{"image_url":{"url":"https://a/1.png"},"role":"reference_image","type":"image_url"},{"image_url":{"url":"https://a/2.png"},"role":"reference_image","type":"image_url"},{"role":"reference_video","type":"video_url","video_url":{"url":"https://a/v.mp4"}},{"audio_url":{"url":"https://a/a.mp3"},"role":"reference_audio","type":"audio_url"}],"duration":8,"model":"MiniMax-H3","ratio":"adaptive","resolution":"2K"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalJSON(t, videoTaskRequest(tc.wire)); got != tc.want {
				t.Fatalf("body drifted:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

// intValue is a local pointer helper for the fixture tables.
func intValue(value int) *int { return &value }
