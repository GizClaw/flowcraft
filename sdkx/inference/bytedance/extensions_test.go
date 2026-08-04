package bytedance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/message/media"
)

// TestGenerateExtensionsCapturedWire lowers every GenerateOptions field and
// asserts its exact destination in the Responses API payload. The thinking
// switch is canonical now; TestGenerateThinkingSwitch covers it.
func TestGenerateExtensionsCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("done"),
		}))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	limit := int64(5)
	maxKeyword := int32(2)
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		withExtensions(simpleTextRequest("hi"), GenerateOptions{
			ServiceTier:        "auto",
			Caching:            &GenerateCaching{Enabled: true, Prefix: true},
			Store:              ptr(true),
			PreviousResponseID: "resp_prev",
			ParallelToolCalls:  ptr(false),
			MaxToolCalls:       ptr(int64(3)),
			WebSearch: &GenerateWebSearch{
				Limit:      &limit,
				MaxKeyword: &maxKeyword,
				Sources:    []string{"toutiao", "search_engine"},
				UserLocation: GenerateWebSearchLocation{
					City:    "Beijing",
					Country: "CN",
				},
			},
		}),
	)
	if err != nil {
		unwrapLog(t, err)
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("empty response")
	}

	body := capture.body(0)
	if body["service_tier"] != "auto" {
		t.Fatalf("service_tier = %v", body["service_tier"])
	}
	caching, _ := body["caching"].(map[string]any)
	if caching["type"] != "enabled" || caching["prefix"] != true {
		t.Fatalf("caching = %v", body["caching"])
	}
	if body["store"] != true {
		t.Fatalf("store = %v", body["store"])
	}
	if body["previous_response_id"] != "resp_prev" {
		t.Fatalf("previous_response_id = %v", body["previous_response_id"])
	}
	if body["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %v", body["parallel_tool_calls"])
	}
	if body["max_tool_calls"].(float64) != 3 {
		t.Fatalf("max_tool_calls = %v", body["max_tool_calls"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	search, _ := tools[0].(map[string]any)
	if search["type"] != "web_search" {
		t.Fatalf("tool = %v", search)
	}
	if search["limit"].(float64) != 5 || search["max_keyword"].(float64) != 2 {
		t.Fatalf("search limits = %v", search)
	}
	sources, _ := search["sources"].([]any)
	if len(sources) != 2 || sources[0] != "toutiao" || sources[1] != "search_engine" {
		t.Fatalf("sources = %v", search["sources"])
	}
	location, _ := search["user_location"].(map[string]any)
	if location["type"] != "approximate" ||
		location["city"] != "Beijing" || location["country"] != "CN" {
		t.Fatalf("location = %v", search["user_location"])
	}
}

// TestGenerateThinkingSwitch covers the canonical reasoning switch: explicit
// disable maps to the disabled enum on the wire, and disable+effort is a
// request validation failure before the compiler ever runs.
func TestGenerateThinkingSwitch(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("done"),
		}))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	disableRequest := simpleTextRequest("hi")
	disableRequest.Input.Content.Intent.Text.ReasoningEnabled = ptr(false)
	_, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		disableRequest,
	)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	thinking, _ := capture.body(0)["thinking"].(map[string]any)
	if thinking["type"] != "disabled" {
		t.Fatalf("thinking = %v", capture.body(0)["thinking"])
	}

	conflictRequest := simpleTextRequest("hi")
	conflictRequest.Input.Content.Intent.Text.ReasoningEnabled = ptr(false)
	conflictRequest.Input.Content.Intent.Text.ReasoningEffort = inference.ReasoningLow
	_, err = runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		conflictRequest,
	)
	if err == nil {
		t.Fatal("expected reasoning switch/effort conflict validation failure")
	}
	if !inference.IsKind(err, inference.InvalidRequest) {
		t.Fatalf("kind = %v", err)
	}
	if len(capture.bodies) != 1 {
		t.Fatalf("transport ran %d times", len(capture.bodies))
	}
}

// TestImageExtensionsCapturedWire lowers ImageOptions onto the images API
// payload, including the grouped-generation single-call path.
func TestImageExtensionsCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, body map[string]any, _ bool) {
		payload, _ := json.Marshal(map[string]any{
			"model":   body["model"],
			"created": 1,
			"data": []map[string]any{
				{"url": "https://example.com/a.png", "size": "2048x2048"},
				{"url": "https://example.com/b.png", "size": "2048x2048"},
			},
			"usage": map[string]any{"generated_images": 2, "output_tokens": 84, "total_tokens": 84},
		})
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, string(payload))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seedream-5-0"),
		withExtensions(imageRequest(0), ImageOptions{
			GuidanceScale: ptr(2.5),
			Watermark:     ptr(false),
			OptimizePrompt: &ImageOptimizePrompt{
				Mode:     "fast",
				Thinking: "disabled",
			},
			Sequential:          ptr(true),
			SequentialMaxImages: ptr(2),
			SizeToken:           "2k",
			WebSearch:           ptr(true),
		}),
	)
	if err != nil {
		unwrapLog(t, err)
		t.Fatalf("Generate: %v", err)
	}
	if len(response.Message.Content.Parts) != 2 {
		t.Fatalf("parts = %d", len(response.Message.Content.Parts))
	}
	// Grouped generation is a single call despite the count intent.
	if len(capture.bodies) != 1 {
		t.Fatalf("requests = %d", len(capture.bodies))
	}

	body := capture.body(0)
	if body["watermark"] != false {
		t.Fatalf("watermark = %v", body["watermark"])
	}
	if body["guidance_scale"].(float64) != 2.5 {
		t.Fatalf("guidance_scale = %v", body["guidance_scale"])
	}
	optimize, _ := body["optimize_prompt_options"].(map[string]any)
	if body["optimize_prompt"] != true ||
		optimize["mode"] != "fast" || optimize["thinking"] != "disabled" {
		t.Fatalf("optimize = %v / %v", body["optimize_prompt"], optimize)
	}
	if body["sequential_image_generation"] != "auto" {
		t.Fatalf("sequential = %v", body["sequential_image_generation"])
	}
	sequential, _ := body["sequential_image_generation_options"].(map[string]any)
	if sequential["max_images"].(float64) != 2 {
		t.Fatalf("max_images = %v", sequential)
	}
	if body["size"] != "2K" {
		t.Fatalf("size = %v", body["size"])
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %v", body["tools"])
	}
	toolEntry, _ := tools[0].(map[string]any)
	if toolEntry["type"] != "web_search" {
		t.Fatalf("tool = %v", toolEntry)
	}
}

// TestImageExtensionConflicts rejects option fields that collide with the
// canonical intent channels.
func TestImageExtensionConflicts(t *testing.T) {
	server, capture := newCapturedArk(t, func(http.ResponseWriter, map[string]any, bool) {
		t.Error("transport must not run after compiler rejection")
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	cases := []struct {
		name   string
		mutate func(*inference.GenerateRequest)
		field  inference.FieldID
	}{
		{
			name: "size token collides with canonical size",
			mutate: func(r *inference.GenerateRequest) {
				r.Input.Content.Intent.Image.Size = &media.ImageSize{
					Width: 1024, Height: 1024,
				}
				r.Extensions = inference.Extensions{
					ImageOptions{SizeToken: "2k"},
				}
			},
			field: "extension.bytedance.image_options.size_token",
		},
		{
			name: "sequential max collides with canonical count",
			mutate: func(r *inference.GenerateRequest) {
				r.Extensions = inference.Extensions{
					ImageOptions{
						Sequential:          ptr(true),
						SequentialMaxImages: ptr(3),
					},
				}
			},
			field: "extension.bytedance.image_options.sequential_max_images",
		},
		{
			name: "generate options do not apply to image models",
			mutate: func(r *inference.GenerateRequest) {
				r.Extensions = inference.Extensions{
					GenerateOptions{Store: ptr(true)},
				}
			},
			field: "extension.bytedance.generate_options.store",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			request := imageRequest(2)
			tc.mutate(&request)
			_, err := runtime.Generate(
				context.Background(),
				generateModel("doubao-seedream-5-0"),
				request,
			)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			if !inference.IsKind(err, inference.InvalidExtension) {
				t.Fatalf("kind = %v", err)
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
		})
	}
	if len(capture.bodies) != 0 {
		t.Fatalf("transport ran %d times", len(capture.bodies))
	}
}

// TestTTSExtensionsCapturedWire lowers TTSOptions into the synthesis payload.
func TestTTSExtensionsCapturedWire(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, ttsChunkLine(0, []byte{1, 2, 3}))
		fmt.Fprint(w, ttsChunkLine(ttsV2CodeStreamDone, nil))
	})
	defer server.Close()
	runtime := newSpeechRuntime(t, server)

	_, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-tts-2-0"),
		withExtensions(
			ttsGenerateRequest(media.AudioFormat{Encoding: media.AudioEncodingMP3}),
			TTSOptions{
				PitchRate:  ptr(5),
				VolumeRate: ptr(-10),
				Emotion:    "happy",
				BitRate:    ptr(128000),
			},
		),
	)
	if err != nil {
		unwrapLog(t, err)
		t.Fatalf("Generate: %v", err)
	}

	params, _ := capture.body(0)["req_params"].(map[string]any)
	audio, _ := params["audio_params"].(map[string]any)
	if audio["pitch_rate"].(float64) != 5 {
		t.Fatalf("pitch_rate = %v", audio["pitch_rate"])
	}
	if audio["volume_rate"].(float64) != -10 {
		t.Fatalf("volume_rate = %v", audio["volume_rate"])
	}
	if audio["emotion"] != "happy" {
		t.Fatalf("emotion = %v", audio["emotion"])
	}
	if audio["bit_rate"].(float64) != 128000 {
		t.Fatalf("bit_rate = %v", audio["bit_rate"])
	}
}

// TestTTSExtensionRejections covers option fields the wire cannot honor.
func TestTTSExtensionRejections(t *testing.T) {
	server, capture := newCapturedArk(t, func(http.ResponseWriter, map[string]any, bool) {
		t.Error("transport must not run after compiler rejection")
	})
	defer server.Close()
	runtime := newSpeechRuntime(t, server)

	cases := []struct {
		name    string
		request inference.GenerateRequest
		field   inference.FieldID
	}{
		{
			name: "bitrate on raw PCM",
			request: withExtensions(
				ttsGenerateRequest(media.AudioFormat{
					Encoding:     media.AudioEncodingPCM16,
					SampleRateHz: 16000,
					Channels:     1,
				}),
				TTSOptions{BitRate: ptr(128000)},
			),
			field: "extension.bytedance.tts_options.bit_rate",
		},
		{
			name: "image options do not apply to speech models",
			request: withExtensions(
				ttsGenerateRequest(media.AudioFormat{Encoding: media.AudioEncodingMP3}),
				ImageOptions{Watermark: ptr(false)},
			),
			field: "extension.bytedance.image_options.watermark",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runtime.Generate(
				context.Background(),
				generateModel("doubao-tts-2-0"),
				tc.request,
			)
			if err == nil {
				t.Fatal("expected compiler rejection")
			}
			if !inference.IsKind(err, inference.InvalidExtension) {
				t.Fatalf("kind = %v", err)
			}
			var inferenceErr *inference.Error
			if !errors.As(err, &inferenceErr) || inferenceErr.Field != tc.field {
				t.Fatalf("field = %v, want %s", err, tc.field)
			}
		})
	}
	if len(capture.bodies) != 0 {
		t.Fatalf("transport ran %d times", len(capture.bodies))
	}
}

// TestExtensionValidation covers request-level extension validation failures.
func TestExtensionValidation(t *testing.T) {
	server, capture := newCapturedArk(t, func(http.ResponseWriter, map[string]any, bool) {
		t.Error("transport must not run after validation failure")
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	_, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		withExtensions(simpleTextRequest("hi"), GenerateOptions{
			ServiceTier: "premium",
		}),
	)
	if err == nil {
		t.Fatal("expected validation failure")
	}
	if len(capture.bodies) != 0 {
		t.Fatalf("transport ran %d times", len(capture.bodies))
	}
}

// withExtensions attaches provider options to a request.
func withExtensions(
	request inference.GenerateRequest,
	extensions ...inference.Extension,
) inference.GenerateRequest {
	request.Extensions = inference.Extensions(extensions)
	return request
}

// imageRequest builds a minimal image-generation request; count <= 1 leaves
// the count intent unset.
func imageRequest(count int) inference.GenerateRequest {
	intent := &inference.ImageIntent{}
	if count > 1 {
		intent.Count = &count
	}
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{
					Parts: []message.Part{message.TextPart{Text: "a red boat"}},
				},
				Intent: inference.Intent{Image: intent},
			},
		},
	}
}

func ptr[T any](value T) *T { return &value }

// foreignProviderExtension stands in for another provider's extension: it
// must stay inert on bytedance attempts so one request can carry several
// providers' settings across route fallback.
type foreignProviderExtension struct{}

func (foreignProviderExtension) ProviderID() string  { return "acme" }
func (foreignProviderExtension) ExtensionID() string { return "acme_options" }
func (foreignProviderExtension) ActiveFields() []inference.ExtensionField {
	return []inference.ExtensionField{"mode"}
}
func (foreignProviderExtension) Validate() error { return nil }
func (e foreignProviderExtension) Clone() inference.Extension {
	return e
}

func TestForeignProviderExtensionIsInertOnBytedanceAttempts(t *testing.T) {
	server, capture := newCapturedArk(t, func(w http.ResponseWriter, _ map[string]any, _ bool) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		}))
	})
	defer server.Close()
	runtime := newTestRuntime(t, server)

	request := withExtensions(
		simpleTextRequest("hi"),
		foreignProviderExtension{},
		&GenerateOptions{ServiceTier: "auto"},
	)
	response, err := runtime.Generate(
		context.Background(),
		generateModel("doubao-seed-2-1-pro"),
		request,
	)
	if err != nil {
		t.Fatalf("Generate with mixed extensions: %v", err)
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("empty response")
	}
	body := capture.body(0)
	if body["service_tier"] != "auto" {
		t.Fatalf("service_tier = %v, own extension not applied", body["service_tier"])
	}
	if _, leaked := body["mode"]; leaked {
		t.Fatalf("foreign extension leaked into the wire: %v", body)
	}
}
