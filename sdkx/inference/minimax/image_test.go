package minimax

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// Image generation pipeline tests, run end to end through the runtime
// against the fake server: the image_generation wire shape including
// subject references, url/base64 deliveries, and the compiler's
// capability gate.

// imageGenerateRequest builds an image request: one user text prompt and
// an image intent carrying the generation knobs.
func imageGenerateRequest(intent inference.ImageIntent) inference.GenerateRequest {
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "a small red boat"}},
				},
				Intent: inference.Intent{Image: &intent},
			},
		},
	}
}

// imageEnvelope renders the image_generation response envelope.
func imageEnvelope(data map[string]any) string {
	payload, _ := json.Marshal(map[string]any{
		"data":      data,
		"id":        "img-1",
		"base_resp": map[string]any{"status_code": 0, "status_msg": "success"},
	})
	return string(payload)
}

func TestImageUnaryCapturedWire(t *testing.T) {
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, imageEnvelope(map[string]any{
			"image_urls": []string{
				"https://example.com/out-1.jpg",
				"https://example.com/out-2.jpg",
			},
		}))
	})
	runtime := newTestRuntime(t, server)

	seed := int64(42)
	count := 2
	reference, err := media.NewImageURL("https://example.com/ref.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	request := imageGenerateRequest(inference.ImageIntent{
		Size:  &media.ImageSize{Width: 1024, Height: 768},
		Count: &count,
		Seed:  &seed,
	})
	request.Input.Content.Parts = append(
		request.Input.Content.Parts, message.ImagePart{Source: reference},
	)
	response, err := runtime.Generate(context.Background(), minimaxModel("image-01"), request)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if response.Metadata.RequestID != "img-1" {
		t.Fatalf("request id = %q, want img-1", response.Metadata.RequestID)
	}
	if len(response.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	for index, part := range response.Message.Content.Parts {
		image, ok := part.(message.ImagePart)
		if !ok {
			t.Fatalf("parts[%d] = %#v", index, part)
		}
		if image.Source.Kind() != media.SourceURL ||
			image.Source.MediaType() != media.ImageFormatJPEG.MediaType() {
			t.Fatalf("parts[%d] source = %+v", index, image.Source)
		}
	}
	first := response.Message.Content.Parts[0].(message.ImagePart)
	if first.Source.URL() != "https://example.com/out-1.jpg" {
		t.Fatalf("url = %q", first.Source.URL())
	}
	if response.Usage.GeneratedImages == nil || *response.Usage.GeneratedImages != 2 {
		t.Fatalf("GeneratedImages = %v", response.Usage.GeneratedImages)
	}
	if server.requests() != 1 {
		t.Fatalf("requests = %d", server.requests())
	}

	body := server.body(t, 0)
	if body["model"] != "image-01" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["prompt"] != "a small red boat" {
		t.Fatalf("prompt = %v", body["prompt"])
	}
	if body["n"] != float64(2) {
		t.Fatalf("n = %v", body["n"])
	}
	if body["response_format"] != "url" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	if _, exists := body["aspect_ratio"]; exists {
		t.Fatalf("aspect_ratio must stay unset: %v", body)
	}
	if body["width"] != float64(1024) || body["height"] != float64(768) {
		t.Fatalf("width/height = %v/%v", body["width"], body["height"])
	}
	if body["seed"] != float64(42) {
		t.Fatalf("seed = %v", body["seed"])
	}
	references, ok := body["subject_reference"].([]any)
	if !ok || len(references) != 1 {
		t.Fatalf("subject_reference = %#v", body["subject_reference"])
	}
	entry, _ := references[0].(map[string]any)
	if entry["type"] != "character" || entry["image_file"] != "https://example.com/ref.png" {
		t.Fatalf("subject_reference[0] = %#v", references[0])
	}
}

// TestImageInlineDelivery asserts the base64 delivery path: the wire
// switches response_format and the payload decodes into inline bytes. The
// aspect-ratio knob rides along (it is mutually exclusive with size, so
// the size path cannot cover it).
func TestImageInlineDelivery(t *testing.T) {
	jpegBytes := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3, 4}
	server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
		_, _ = fmt.Fprint(w, imageEnvelope(map[string]any{
			"image_base64": []string{base64.StdEncoding.EncodeToString(jpegBytes)},
		}))
	})
	runtime := newTestRuntime(t, server)

	response, err := runtime.Generate(
		context.Background(),
		minimaxModel("image-01"),
		imageGenerateRequest(inference.ImageIntent{
			AspectRatio: "16:9",
			Delivery:    media.SourceInline,
		}),
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if response.Metadata.RequestID != "img-1" {
		t.Fatalf("request id = %q, want img-1", response.Metadata.RequestID)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.Kind() != media.SourceInline {
		t.Fatalf("kind = %v", part.Source.Kind())
	}
	if string(part.Source.Bytes()) != string(jpegBytes) {
		t.Fatalf("bytes = %v", part.Source.Bytes())
	}
	if part.Source.MediaType() != media.ImageFormatJPEG.MediaType() {
		t.Fatalf("media type = %q", part.Source.MediaType())
	}

	body := server.body(t, 0)
	if body["response_format"] != "base64" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	if body["aspect_ratio"] != "16:9" {
		t.Fatalf("aspect_ratio = %v", body["aspect_ratio"])
	}
}

func TestImageRejections(t *testing.T) {
	count := 10
	cases := []struct {
		name   string
		mutate func(*inference.GenerateRequest)
		field  inference.FieldID
	}{
		{
			name: "size not divisible by eight",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Image.Size = &media.ImageSize{Width: 513, Height: 513}
			},
			field: inference.FieldGenerateIntentImageSize,
		},
		{
			name: "count above nine",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Image.Count = &count
			},
			field: inference.FieldGenerateIntentImageCount,
		},
		{
			name: "png output format",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Image.OutputFormat = media.ImageFormatPNG
			},
			field: inference.FieldGenerateIntentImageOutputFormat,
		},
		{
			name: "unsupported aspect ratio",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Image.AspectRatio = "5:4"
			},
			field: inference.FieldGenerateIntentImageAspectRatio,
		},
		{
			name: "audio intent",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Audio = &inference.AudioIntent{
					Voice:  media.VoiceSpec{ID: "male-qn-qingse"},
					Format: media.AudioFormat{Encoding: media.AudioEncodingMP3},
				}
			},
			field: inference.FieldGenerateIntentAudio,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newMessagesServer(t, func(w http.ResponseWriter, _ map[string]any) {
				t.Error("transport must not run after compiler rejection")
			})
			runtime := newTestRuntime(t, server)
			request := imageGenerateRequest(inference.ImageIntent{})
			tc.mutate(&request)
			_, err := runtime.Generate(
				context.Background(),
				minimaxModel("image-01"),
				request,
			)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
			if !inference.IsKind(err, inference.UnsupportedFeature) {
				t.Fatalf("kind = %v", err)
			}
			if server.requests() != 0 {
				t.Fatalf("transport ran %d times", server.requests())
			}
		})
	}
}
