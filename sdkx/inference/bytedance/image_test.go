package bytedance

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

var pngHeader = []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}

func TestImageCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, body map[string]any, _ bool) {
		format, _ := body["response_format"].(string)
		var image map[string]any
		if format == "b64_json" {
			image = map[string]any{
				"b64_json": base64.StdEncoding.EncodeToString(pngHeader),
				"size":     "1024x1024",
			}
		} else {
			image = map[string]any{
				"url":  "https://example.com/out.png",
				"size": "1024x1024",
			}
		}
		payload, _ := json.Marshal(map[string]any{
			"model":   body["model"],
			"created": 1,
			"data":    []map[string]any{image},
			"usage":   map[string]any{"generated_images": 1, "output_tokens": 42, "total_tokens": 42},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	seed := int64(7)
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seedream-5-0"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "a small red boat"}},
					},
					Intent: inference.Intent{
						Image: &inference.ImageIntent{
							Size:         &media.ImageSize{Width: 2048, Height: 2048},
							Seed:         &seed,
							OutputFormat: media.ImageFormatPNG,
							Delivery:     media.SourceInline,
						},
					},
				},
			},
		},
	)
	if err != nil {
		for depth, current := 0, err; current != nil && depth < 8; depth++ {
			t.Logf("depth %d: %T: %v", depth, current, current)
			current = errors.Unwrap(current)
		}
		t.Fatalf("Generate: %v", err)
	}
	if response.FinishReason != inference.FinishCompleted {
		t.Fatalf("finish = %q", response.FinishReason)
	}
	if len(response.Message.Content.Parts) != 1 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	part, ok := response.Message.Content.Parts[0].(message.ImagePart)
	if !ok {
		t.Fatalf("part = %#v", response.Message.Content.Parts[0])
	}
	if part.Source.Kind() != media.SourceInline ||
		part.Source.MediaType() != "image/png" {
		t.Fatalf("source = %+v", part.Source)
	}
	if response.Usage.TotalTokens != 42 {
		t.Fatalf("usage = %+v", response.Usage)
	}

	body := capture.body(0)
	if body["model"] != "doubao-seedream-5-0" {
		t.Fatalf("model = %v", body["model"])
	}
	if body["prompt"] != "a small red boat" {
		t.Fatalf("prompt = %v", body["prompt"])
	}
	if body["size"] != "2048x2048" {
		t.Fatalf("size = %v", body["size"])
	}
	if body["seed"].(float64) != 7 {
		t.Fatalf("seed = %v", body["seed"])
	}
	if body["response_format"] != "b64_json" {
		t.Fatalf("response_format = %v", body["response_format"])
	}
	if body["output_format"] != "png" {
		t.Fatalf("output_format = %v", body["output_format"])
	}
}

func TestImageCountFanout(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		payload, _ := json.Marshal(map[string]any{
			"created": 1,
			"data":    []map[string]any{{"url": "https://example.com/out.png"}},
			"usage":   map[string]any{"generated_images": 1, "output_tokens": 10, "total_tokens": 10},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	count := 3
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seedream-5-0"),
		inference.GenerateRequest{
			Input: inference.GenerateInput{
				Role: inference.InputRoleUser,
				Content: inference.InputContent{
					Content: message.Content{
						Parts: []message.Part{message.TextPart{Text: "three boats"}},
					},
					Intent: inference.Intent{Image: &inference.ImageIntent{Count: &count}},
				},
			},
		},
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 3 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	if len(capture.bodies) != 3 {
		t.Fatalf("calls = %d", len(capture.bodies))
	}
	if response.Usage.TotalTokens != 30 {
		t.Fatalf("usage = %+v", response.Usage)
	}
}

func TestImageRejections(t *testing.T) {
	seed := int64(1)
	count := 2
	cases := []struct {
		name   string
		intent inference.ImageIntent
		field  inference.FieldID
	}{
		{
			name:   "aspect ratio",
			intent: inference.ImageIntent{AspectRatio: "16:9"},
			field:  inference.FieldGenerateIntentImageAspectRatio,
		},
		{
			name:   "seed with count",
			intent: inference.ImageIntent{Seed: &seed, Count: &count},
			field:  inference.FieldGenerateIntentImageSeed,
		},
		{
			name:   "gif format",
			intent: inference.ImageIntent{OutputFormat: media.ImageFormatGIF},
			field:  inference.FieldGenerateIntentImageOutputFormat,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
				t.Error("transport must not run after compiler rejection")
			})
			defer server.Close()
			runtime := newTestRuntime(t, server)
			request := inference.GenerateRequest{
				Input: inference.GenerateInput{
					Role: inference.InputRoleUser,
					Content: inference.InputContent{
						Content: message.Content{
							Parts: []message.Part{message.TextPart{Text: "x"}},
						},
						Intent: inference.Intent{Image: &tc.intent},
					},
				},
			}
			_, err := runtime.Generate(
				context.Background(),
				generateModel("doubao-seedream-5-0"),
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
			if len(capture.bodies) != 0 {
				t.Fatalf("transport ran %d times", len(capture.bodies))
			}
		})
	}
}
