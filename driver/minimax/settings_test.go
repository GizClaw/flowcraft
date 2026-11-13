package minimax

import (
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

func testSpec(name, kind string, inputs, outputs []message.PartKind) ModelSpec {
	return ModelSpec{
		Name: name, Kind: kind,
		Capabilities: model.ModelCapabilities{Inputs: inputs, Outputs: outputs},
	}
}

func TestSettingsRequireAPIKey(t *testing.T) {
	settings := Settings{Models: []ModelSpec{
		testSpec("speech-2.6-hd", "tts", []message.PartKind{message.PartText},
			[]message.PartKind{message.PartAudio}),
	}}
	if err := settings.Validate(); err == nil {
		t.Fatal("settings without an api key were accepted")
	}
	settings.APIKey = "key"
	if err := settings.Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
}

func TestModelKindFamilyContract(t *testing.T) {
	cases := []struct {
		name string
		spec ModelSpec
		want string
	}{
		{
			name: "tts without audio output",
			spec: testSpec("speech", "tts", []message.PartKind{message.PartText},
				[]message.PartKind{message.PartText}),
			want: "audio output",
		},
		{
			name: "video without video output",
			spec: testSpec("MiniMax-H3", "video", []message.PartKind{message.PartText},
				[]message.PartKind{message.PartAudio}),
			want: "video output",
		},
		{
			name: "kind the driver does not serve",
			spec: testSpec("image-01", "image", []message.PartKind{message.PartText},
				[]message.PartKind{message.PartImage}),
			want: "unsupported kind",
		},
		{
			name: "embedding width on a media model",
			spec: func() ModelSpec {
				spec := testSpec("m", "video", []message.PartKind{message.PartText},
					[]message.PartKind{message.PartVideo})
				spec.Capabilities.EmbeddingDimensions = 1024
				return spec
			}(),
			want: "no embedding surface",
		},
		{
			name: "speech parameters on a video model",
			spec: func() ModelSpec {
				spec := testSpec("MiniMax-H3", "video", []message.PartKind{message.PartText},
					[]message.PartKind{message.PartVideo})
				spec.TTS = TTSParams{Encoding: media.AudioEncodingMP3}
				return spec
			}(),
			want: "declares tts parameters",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("declaration was accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTTSParamsValidate(t *testing.T) {
	cases := []struct {
		name string
		tts  TTSParams
		ok   bool
	}{
		{name: "undeclared defaults to the platform", tts: TTSParams{}, ok: true},
		{name: "mp3", tts: TTSParams{Encoding: media.AudioEncodingMP3}, ok: true},
		{
			name: "pcm16 with its shape",
			tts:  TTSParams{Encoding: media.AudioEncodingPCM16, SampleRateHz: 24000, Channels: 1},
			ok:   true,
		},
		{
			name: "pcm16 without its shape",
			tts:  TTSParams{Encoding: media.AudioEncodingPCM16},
		},
		{
			name: "sample rate without an encoding",
			tts:  TTSParams{SampleRateHz: 24000},
		},
		{
			name: "unsupported encoding",
			tts:  TTSParams{Encoding: media.AudioEncodingAAC},
		},
		{
			name: "unsupported sample rate",
			tts: TTSParams{
				Encoding: media.AudioEncodingPCM16, SampleRateHz: 48000, Channels: 1,
			},
		},
		{
			name: "more channels than stereo",
			tts:  TTSParams{Encoding: media.AudioEncodingMP3, Channels: 6},
		},
		{name: "negative prompt limit", tts: TTSParams{PromptLimit: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.tts.Validate()
			if tc.ok && err != nil {
				t.Fatalf("declaration rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("declaration accepted")
			}
		})
	}
}

func TestDuplicateDeclarationsRejected(t *testing.T) {
	spec := testSpec("speech-2.6-hd", "tts", []message.PartKind{message.PartText},
		[]message.PartKind{message.PartAudio})
	settings := Settings{APIKey: "key", Models: []ModelSpec{spec, spec}}
	if err := settings.Validate(); err == nil {
		t.Fatal("duplicate declarations were accepted")
	}
}

func TestCatalogPublishesDeclaredFacts(t *testing.T) {
	declared := testSpec("MiniMax-H3", "video",
		[]message.PartKind{message.PartText, message.PartImage},
		[]message.PartKind{message.PartVideo})
	declared.Limits.MaxInputTokens = nil
	entry, ok := newCatalog([]ModelSpec{declared}).Describe(model.ModelID{
		Provider: ProviderID, Name: "MiniMax-H3",
	})
	if !ok {
		t.Fatal("declared model was not published")
	}
	if len(entry.Capabilities.Outputs) != 1 ||
		entry.Capabilities.Outputs[0] != message.PartVideo {
		t.Fatalf("outputs = %v, want video", entry.Capabilities.Outputs)
	}
	if _, found := newCatalog([]ModelSpec{declared}).Describe(model.ModelID{
		Provider: ProviderID, Name: "MiniMax-Hailuo-02",
	}); found {
		t.Fatal("undeclared model was published")
	}
}

func TestSettingsNormalized(t *testing.T) {
	settings := Settings{}.normalized()
	if settings.BaseURL != DefaultBaseURL {
		t.Fatalf("base url = %q, want %q", settings.BaseURL, DefaultBaseURL)
	}
	if settings.Timeout != 10*time.Minute {
		t.Fatalf("timeout = %v", settings.Timeout)
	}
	if settings.VideoPollInterval != DefaultVideoPollInterval {
		t.Fatalf("poll interval = %v", settings.VideoPollInterval)
	}
	// A trailing slash is the most likely paste from a gateway console; it must
	// not survive into c.base+path.
	trimmed := Settings{BaseURL: "https://gw.example.com/"}.normalized()
	if trimmed.BaseURL != "https://gw.example.com" {
		t.Fatalf("base url = %q, want the trailing slash trimmed", trimmed.BaseURL)
	}
	// A non-positive poll interval keeps the default rather than erroring, so
	// Validate has no negative value to reject.
	for _, value := range []time.Duration{0, -time.Second} {
		kept := Settings{VideoPollInterval: value}.normalized()
		if kept.VideoPollInterval != DefaultVideoPollInterval {
			t.Fatalf("poll interval %v normalized to %v", value, kept.VideoPollInterval)
		}
		if err := kept.Validate(); err != nil && strings.Contains(err.Error(), "poll") {
			t.Fatalf("poll interval %v produced a validation error: %v", value, err)
		}
	}
}

func TestSettingsRejectVersionedBaseURL(t *testing.T) {
	// The driver appends /v1/t2a_v2 and /v2/video_generation, so a base that
	// already carries a version segment would produce /v1/v2/video_generation.
	for _, base := range []string{
		"https://gw.example.com/v1",
		"https://gw.example.com/v1/",
		"https://gw.example.com/v2",
	} {
		settings := Settings{BaseURL: base, APIKey: "key"}
		if err := settings.Validate(); err == nil {
			t.Fatalf("base url %q was accepted", base)
		}
	}
	// A host root passes, including one with a real path prefix that is not a
	// version segment.
	for _, base := range []string{
		"https://gw.example.com",
		"https://gw.example.com/minimax",
		"http://localhost:8080",
	} {
		settings := Settings{BaseURL: base, APIKey: "key"}
		if err := settings.Validate(); err != nil {
			t.Fatalf("base url %q rejected: %v", base, err)
		}
	}
}

func TestVideoParamsValidate(t *testing.T) {
	minDuration := func(value int) *int { return &value }
	cases := []struct {
		name   string
		params VideoParams
		ok     bool
	}{
		{name: "empty keeps the platform envelope", params: VideoParams{}, ok: true},
		{name: "narrowed range", params: VideoParams{
			DurationMin: minDuration(6), DurationMax: minDuration(10),
		}, ok: true},
		{name: "negative prompt limit", params: VideoParams{PromptLimit: -1}},
		{name: "duration min below the envelope", params: VideoParams{DurationMin: minDuration(3)}},
		{name: "duration max above the envelope", params: VideoParams{DurationMax: minDuration(16)}},
		{name: "empty duration range", params: VideoParams{
			DurationMin: minDuration(10), DurationMax: minDuration(6),
		}},
		{name: "range excluding the default", params: VideoParams{
			DurationMin: minDuration(10), DurationMax: minDuration(15),
		}},
		{name: "unknown resolution", params: VideoParams{Resolutions: []string{"1080P"}}},
		{name: "resolutions without the default tier", params: VideoParams{
			Resolutions: []string{"2K"},
		}},
		{name: "unknown ratio", params: VideoParams{Ratios: []string{"2:1"}}},
		{name: "ratios without the text default", params: VideoParams{Ratios: []string{"9:16"}}},
		{name: "reference images above the platform cap", params: VideoParams{
			ReferenceImages: referenceImageMax + 1,
		}},
		{name: "negative reference videos", params: VideoParams{ReferenceVideos: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.params.Validate()
			if tc.ok && err != nil {
				t.Fatalf("declaration rejected: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("declaration accepted")
			}
		})
	}
}

func TestVideoParamsBelongToTheVideoKind(t *testing.T) {
	spec := testSpec("speech-2.6-hd", "tts", []message.PartKind{message.PartText},
		[]message.PartKind{message.PartAudio})
	spec.Video = VideoParams{PromptLimit: 10}
	if err := spec.Validate(); err == nil {
		t.Fatal("video parameters on a tts model were accepted")
	}
}
