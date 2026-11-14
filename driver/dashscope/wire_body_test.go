package dashscope

import (
	stdjson "encoding/json"
	"testing"
)

// canonicalBody renders a request body with sorted map keys, so a golden asserts
// what the wire says rather than the order a struct happens to emit it in.
func canonicalBody(t *testing.T, v any) string {
	t.Helper()
	raw, err := stdjson.Marshal(v)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	var generic any
	if err := stdjson.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	out, err := stdjson.Marshal(generic)
	if err != nil {
		t.Fatalf("re-encode body: %v", err)
	}
	return string(out)
}

// TestWireBodies pins every request body this driver sends. The tables are the
// contract: a field that gains or loses presence here changes what the platform is
// asked to do, so the golden must move deliberately.
func TestWireBodies(t *testing.T) {
	yes, no := true, false
	dimension := 1024
	seed := int64(7)
	duration := 5

	cases := []struct {
		name string
		body any
		want string
	}{
		{
			name: "embed minimal",
			body: embedRequest(embedWire{model: "m"}, []string{"a", "b"}),
			want: `{"input":{"texts":["a","b"]},"model":"m"}`,
		},
		{
			name: "embed parameters",
			body: embedRequest(embedWire{model: "m", dimension: &dimension, textType: "query", instruct: "ins"}, []string{"a"}),
			want: `{"input":{"texts":["a"]},"model":"m","parameters":{"dimension":1024,"instruct":"ins","text_type":"query"}}`,
		},
		{
			name: "tts minimal",
			body: ttsRequest(ttsWire{model: "m", text: "t", voice: "v"}),
			want: `{"input":{"text":"t","voice":"v"},"model":"m"}`,
		},
		{
			name: "tts language",
			body: ttsRequest(ttsWire{model: "m", text: "t", voice: "v", language: "Chinese"}),
			want: `{"input":{"language_type":"Chinese","text":"t","voice":"v"},"model":"m"}`,
		},
		{
			name: "transcribe keeps an empty parameters block",
			body: transcribeRequest(transcribeWire{model: "m", fileURL: "https://a/f.wav"}),
			want: `{"input":{"file_urls":["https://a/f.wav"]},"model":"m","parameters":{}}`,
		},
		{
			name: "image text-to-image",
			body: imageTaskRequest(imageWire{model: "m", prompt: "p", count: 1}),
			want: `{"input":{"prompt":"p"},"model":"m","parameters":{"n":1}}`,
		},
		{
			name: "image image-to-image",
			body: imageTaskRequest(imageWire{model: "m", path: imagePathImage2Image,
				prompt: "p", count: 2, images: []string{"https://a/i.png"}, size: "1024*1024",
				seed: &seed, negative: "n", promptExtend: &no, watermark: &yes}),
			want: `{"input":{"base_image_url":"https://a/i.png","prompt":"p"},"model":"m","parameters":{"n":2,"negative_prompt":"n","prompt_extend":false,"seed":7,"size":"1024*1024","watermark":true}}`,
		},
		{
			name: "video legacy fields",
			body: videoTaskRequest(videoWire{model: "m", prompt: "p",
				image: "https://a/f.png", resolution: "720P", duration: &duration, seed: int64Ptr(9),
				watermark: &no, negative: "neg", promptExtend: &yes, shotType: "single",
				audio: &yes, template: "tpl"}),
			want: `{"input":{"img_url":"https://a/f.png","prompt":"p"},"model":"m","parameters":{"audio":true,"duration":5,"negative_prompt":"neg","prompt_extend":true,"resolution":"720P","seed":9,"shot_type":"single","template":"tpl","watermark":false}}`,
		},
		{
			name: "video media array",
			body: videoTaskRequest(videoWire{model: "m", prompt: "p", mediaMode: true,
				media:      []mediaEntry{{Type: "first_frame", URL: "https://a/1.png"}, {Type: "link", URL: "https://a/l"}},
				resolution: "1080P", duration: &duration}),
			want: `{"input":{"media":[{"type":"first_frame","url":"https://a/1.png"},{"type":"link","url":"https://a/l"}],"prompt":"p"},"model":"m","parameters":{"duration":5,"resolution":"1080P"}}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalBody(t, tc.body); got != tc.want {
				t.Fatalf("body drifted:\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func int64Ptr(v int64) *int64 { return &v }
