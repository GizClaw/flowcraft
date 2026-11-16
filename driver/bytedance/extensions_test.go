package bytedance

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/inferencetest"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// extPtr is the pointer helper these tables read better with. `internal/ptr`
// clones a pointer but cannot make one, so a literal needs this.
func extPtr[T any](value T) *T { return &value }

// Validate is the gate a deployment document passes before a request is built,
// so each rule gets a case: a value the platform would reject is better refused
// with a sentence than sent and answered with an opaque 400.
func TestExtensionValidationRefuses(t *testing.T) {
	cases := []struct {
		name      string
		extension inference.Extension
		want      string
	}{
		{"image count", ImageOptions{Count: extPtr(0)}, "count must be positive"},
		{"image format", ImageOptions{OutputFormat: media.ImageFormatGIF},
			`image format "gif" is not supported`},
		{"image delivery", ImageOptions{Delivery: "base64"}, `unknown image delivery "base64"`},
		{"image guidance", ImageOptions{GuidanceScale: extPtr(0.0)}, "guidance_scale must be positive"},
		{"optimize mode", ImageOptions{OptimizePrompt: &ImageOptimizePrompt{Mode: "turbo"}},
			`optimize_prompt mode must be standard or fast, not "turbo"`},
		{"optimize thinking", ImageOptions{OptimizePrompt: &ImageOptimizePrompt{Thinking: "maybe"}},
			`optimize_prompt thinking must be auto, enabled, or disabled, not "maybe"`},
		{"sequential floor", ImageOptions{SequentialMaxImages: extPtr(0)},
			"sequential_max_images must be in [1, 15], not 0"},
		{"sequential ceiling", ImageOptions{SequentialMaxImages: extPtr(16)},
			"sequential_max_images must be in [1, 15], not 16"},
		{"sequential without the flag", ImageOptions{SequentialMaxImages: extPtr(3)},
			"sequential_max_images requires sequential generation"},
		{"size token", ImageOptions{SizeToken: "8k"},
			`size_token must be 1k, 1.5k, 2k, 3k, 4k, or adaptive, not "8k"`},
		{"background", ImageOptions{Background: "checkerboard"},
			`background must be transparent or opaque, not "checkerboard"`},
		{"layers with a background", ImageOptions{
			LayerDecomposition: extPtr(true), Background: "transparent",
		}, "background and layer decomposition are mutually exclusive"},
		{"video duration", VideoOptions{DurationMillis: extPtr(int64(0))},
			"duration_millis must be positive"},
		{"video seed low", VideoOptions{Seed: extPtr(int64(-2))},
			"seed must be within [-1, 2147483647]"},
		{"service tier", VideoOptions{ServiceTier: "spot"},
			`service_tier must be default or flex, not "spot"`},
		{"expiry floor", VideoOptions{ExecutionExpiresAfter: extPtr(int64(60))},
			"execution_expires_after must be within [3600, 259200], not 60"},
		{"expiry ceiling", VideoOptions{ExecutionExpiresAfter: extPtr(int64(300000))},
			"execution_expires_after must be within [3600, 259200], not 300000"},
		{"priority", VideoOptions{Priority: extPtr(int32(10))},
			"priority must be within [0, 9], not 10"},
		{"output format", VideoOptions{OutputFormat: extPtr("avi")},
			`output_format must be mp4 or mov, not "avi"`},
		{"omni task type", VideoOptions{OmniReferenceTaskType: extPtr("blend")},
			`omni_reference_task_type must be auto, reference, edit, or extend, not "blend"`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			err := test.extension.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", test.name)
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want it to name %q", err, test.want)
			}
		})
	}
}

// The same declarations, written the way a working deployment writes them.
func TestExtensionValidationAccepts(t *testing.T) {
	valid := []inference.Extension{
		ImageOptions{
			Count: extPtr(2), OutputFormat: media.ImageFormatJPEG, Delivery: media.SourceInline,
			GuidanceScale: extPtr(5.5), Sequential: extPtr(true), SequentialMaxImages: extPtr(4),
			SizeToken: "2k", WebSearch: extPtr(true),
			OptimizePrompt: &ImageOptimizePrompt{Mode: "fast", Thinking: "disabled"},
		},
		ImageOptions{LayerDecomposition: extPtr(true), SizeToken: "adaptive"},
		ImageOptions{Background: "transparent"},
		VideoOptions{
			DurationMillis: extPtr(int64(5000)), Seed: extPtr(int64(-1)),
			ServiceTier: "flex", ExecutionExpiresAfter: extPtr(int64(3600)),
			Priority: extPtr(int32(9)), OutputFormat: extPtr("mov"),
			OmniReferenceTaskType: extPtr("edit"),
		},
	}
	for _, extension := range valid {
		if err := extension.Validate(); err != nil {
			t.Errorf("%T %s refused: %v", extension, extension.ExtensionID(), err)
		}
	}
}

// The contract every extension owes — an addressable identity, an ActiveFields
// that reports exactly the options it sets, a Validate that accepts its own
// sample, and a Clone that owns its storage — is verified by the shared suite
// rather than re-derived here. What stays in this file is this driver's own
// business: which declarations the platform refuses.
func TestExtensionContract(t *testing.T) {
	image := ImageOptions{
		Size:                &media.ImageSize{Width: 2048, Height: 2048},
		Count:               extPtr(2),
		Seed:                extPtr(int64(42)),
		OutputFormat:        media.ImageFormatJPEG,
		Delivery:            media.SourceInline,
		Quality:             media.ImageQualityHigh,
		GuidanceScale:       extPtr(5.5),
		Watermark:           extPtr(false),
		OptimizePrompt:      &ImageOptimizePrompt{Mode: "fast", Thinking: "enabled"},
		Sequential:          extPtr(true),
		SequentialMaxImages: extPtr(4),
		SizeToken:           "2k",
		WebSearch:           extPtr(true),
		Background:          "transparent",
	}
	// Layering and a background are mutually exclusive, so the one option this
	// sample cannot carry gets its own.
	layered := ImageOptions{LayerDecomposition: extPtr(true), SizeToken: "adaptive"}
	video := VideoOptions{
		DurationMillis:        extPtr(int64(5000)),
		Resolution:            "720p",
		AspectRatio:           "16:9",
		Seed:                  extPtr(int64(7)),
		Watermark:             extPtr(true),
		CameraFixed:           extPtr(false),
		GenerateAudio:         extPtr(true),
		ServiceTier:           "flex",
		ExecutionExpiresAfter: extPtr(int64(3600)),
		Priority:              extPtr(int32(3)),
		OutputFormat:          extPtr("mov"),
		OmniReferenceTaskType: extPtr("edit"),
		WebSearch:             extPtr(true),
		CallbackURL:           extPtr("https://example.test/hook"),
		SafetyIdentifier:      extPtr("tenant-7"),
	}
	inferencetest.RunExtensions(t,
		inferencetest.ExtensionSample{
			Name:  image.ExtensionID(),
			Value: &image,
			Fields: []inference.ExtensionField{
				"size", "count", "seed", "output_format", "delivery", "quality",
				"guidance_scale", "watermark", "optimize_prompt", "sequential",
				"sequential_max_images", "size_token", "web_search", "background",
			},
		},
		inferencetest.ExtensionSample{
			Name:   image.ExtensionID() + "-layered",
			Value:  &layered,
			Fields: []inference.ExtensionField{"layer_decomposition", "size_token"},
		},
		inferencetest.ExtensionSample{
			Name:  video.ExtensionID(),
			Value: &video,
			Fields: []inference.ExtensionField{
				"duration_millis", "resolution", "aspect_ratio", "seed", "watermark",
				"camera_fixed", "generate_audio", "service_tier",
				"execution_expires_after", "priority", "output_format",
				"omni_reference_task_type", "web_search", "callback_url",
				"safety_identifier",
			},
		},
	)
}

// The document key an extension lands under is namespaced by provider, so a
// deployment that names one gets its own namespace and a plain document gets this
// driver's.
func TestExtensionProvider(t *testing.T) {
	if got := (ImageOptions{}).ProviderID(); got != ProviderID {
		t.Errorf("provider = %q, want %q", got, ProviderID)
	}
	if got := (ImageOptions{Provider: "custom"}).ProviderID(); got != "custom" {
		t.Errorf("provider = %q, want custom", got)
	}
}

// The runtime clones an extension set before it builds a request
// ([inference.Extensions.Clone], which a graph node's model config and
// GenerateRequest.Clone both use). A clone has to stay the type this driver
// asserts on, or a deployment's own options stop being understood the moment the
// framework copies them.
func TestAClonedExtensionIsStillUnderstood(t *testing.T) {
	cloned := inference.Extensions{&ImageOptions{Count: extPtr(2)}}.Clone()
	options, err := imageOptions(cloned)
	if err != nil {
		t.Fatalf("a cloned extension was not understood: %s", causeText(err))
	}
	if options.Count == nil || *options.Count != 2 {
		t.Errorf("count = %v, want the clone's 2", options.Count)
	}
}
