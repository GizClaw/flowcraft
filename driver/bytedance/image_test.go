package bytedance

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

func compileImageRequest(parts []message.Part, options ImageOptions) inference.GenerateRequest {
	var extensions inference.Extensions
	extensions = append(extensions, options)
	return inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: parts},
				Intent:  inference.Intent{Image: &inference.ImageIntent{}},
			},
		},
		Extensions: extensions,
	}
}

func imageReferencePart(t *testing.T) message.ImagePart {
	t.Helper()
	source, err := media.NewImageURL("https://example.com/input.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return message.ImagePart{Source: source}
}

// imageTransportRequest builds the request one transport attempt posts, so the
// transport tests can drive it directly without going through the compiler.
func imageTransportRequest(model, prompt string) *imageRequest {
	delivery := "url"
	return &imageRequest{
		ark: arkmodel.GenerateImagesRequest{
			Model:          model,
			Prompt:         prompt,
			ResponseFormat: &delivery,
		},
		count: 1,
	}
}

func compileImageWire(
	t *testing.T,
	request inference.GenerateRequest,
) (*imageRequest, inference.CompileReport, error) {
	t.Helper()
	compiled, err := compileImage("ep-test")(
		context.Background(),
		model.ModelRef{ID: model.ModelID{Provider: providerID, Name: "seedream-5-0-pro"}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		return nil, compiled.Report, err
	}
	return compiled.Wire, compiled.Report, nil
}

// rejectedReason returns the ledger reason for one rejected field, or ""
// when the compile did not reject it.
func rejectedReason(report inference.CompileReport, field inference.FieldID) string {
	for _, decision := range report.Decisions {
		if decision.Field == field && decision.Disposition == inference.Rejected {
			return decision.Reason
		}
	}
	return ""
}

func TestCompileImageLayerDecomposition(t *testing.T) {
	enabled := true
	wire, _, err := compileImageWire(
		t,
		compileImageRequest(
			[]message.Part{imageReferencePart(t)},
			ImageOptions{LayerDecomposition: &enabled},
		),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.ark.LayerDecomposition == nil || !*wire.ark.LayerDecomposition {
		t.Fatal("layer_decomposition not set on the request")
	}
}

func TestCompileImageLayerDecompositionRejects(t *testing.T) {
	enabled := true
	size := media.ImageSize{Width: 1024, Height: 1024}
	cases := []struct {
		name    string
		request inference.GenerateRequest
		reason  string
	}{
		{
			name: "no reference image",
			request: compileImageRequest(
				[]message.Part{message.TextPart{Text: "decompose"}},
				ImageOptions{LayerDecomposition: &enabled},
			),
			reason: "layer decomposition requires an input image",
		},
		{
			name: "canonical size",
			request: func() inference.GenerateRequest {
				request := compileImageRequest(
					[]message.Part{imageReferencePart(t)},
					ImageOptions{LayerDecomposition: &enabled},
				)
				request.Input.Content.Intent.Image.Size = &size
				return request
			}(),
			reason: "resolution levels only",
		},
		{
			name: "sequential conflict",
			request: compileImageRequest(
				[]message.Part{imageReferencePart(t)},
				ImageOptions{
					LayerDecomposition: &enabled,
					Sequential:         &enabled,
				},
			),
			reason: "not supported with sequential generation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, report, err := compileImageWire(t, tc.request)
			if err == nil {
				t.Fatalf("compile error = nil, want rejection")
			}
			field := inference.ExtensionField("layer_decomposition").Qualify(ImageOptions{})
			if reason := rejectedReason(report, field); !strings.Contains(reason, tc.reason) {
				t.Fatalf("rejected reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileImageBackground(t *testing.T) {
	wire, _, err := compileImageWire(
		t,
		compileImageRequest(
			[]message.Part{imageReferencePart(t)},
			ImageOptions{Background: "transparent"},
		),
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if wire.background != "transparent" {
		t.Fatalf("background = %q, want transparent", wire.background)
	}
}

func TestCompileImageBackgroundRejects(t *testing.T) {
	enabled := true
	second, err := media.NewImageURL("https://example.com/input2.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	cases := []struct {
		name    string
		request inference.GenerateRequest
		reason  string
	}{
		{
			name: "no reference image",
			request: compileImageRequest(
				[]message.Part{message.TextPart{Text: "edit"}},
				ImageOptions{Background: "opaque"},
			),
			reason: "exactly one input image",
		},
		{
			name: "two reference images",
			request: compileImageRequest(
				[]message.Part{
					message.ImagePart{Source: mustImageSource(t)},
					message.ImagePart{Source: second},
				},
				ImageOptions{Background: "opaque"},
			),
			reason: "exactly one input image",
		},
		{
			name: "sequential conflict",
			request: compileImageRequest(
				[]message.Part{imageReferencePart(t)},
				ImageOptions{Background: "opaque", Sequential: &enabled},
			),
			reason: "not supported with sequential generation",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, report, err := compileImageWire(t, tc.request)
			if err == nil {
				t.Fatalf("compile error = nil, want rejection")
			}
			field := inference.ExtensionField("background").Qualify(ImageOptions{})
			if reason := rejectedReason(report, field); !strings.Contains(reason, tc.reason) {
				t.Fatalf("rejected reason = %q, want %q", reason, tc.reason)
			}
		})
	}
}

func TestCompileImageQualityDrops(t *testing.T) {
	request := compileImageRequest(
		[]message.Part{message.TextPart{Text: "a red circle"}},
		ImageOptions{},
	)
	request.Input.Content.Intent.Image.Quality = media.ImageQualityHigh
	_, report, err := compileImageWire(t, request)
	if err != nil {
		t.Fatalf("compile: %v, want quality dropped with success", err)
	}
	found := false
	for _, decision := range report.Decisions {
		if decision.Field == inference.FieldGenerateIntentImageQuality &&
			decision.Disposition == inference.Dropped &&
			strings.Contains(decision.Reason, "no quality parameter") {
			found = true
		}
	}
	if !found {
		t.Fatalf("report decisions = %+v, want quality dropped with reason",
			report.Decisions)
	}
}

func mustImageSource(t *testing.T) media.ImageSource {
	t.Helper()
	source, err := media.NewImageURL("https://example.com/input1.png", "image/png")
	if err != nil {
		t.Fatalf("NewImageURL: %v", err)
	}
	return source
}

func TestCompileImageSizeToken(t *testing.T) {
	for _, tc := range []struct {
		token string
		want  string
	}{
		{"1k", "1K"},
		{"1.5k", "1.5K"},
		{"3k", "3K"},
		{"4k", "4K"},
	} {
		wire, _, err := compileImageWire(
			t,
			compileImageRequest(nil, ImageOptions{SizeToken: tc.token}),
		)
		if err != nil {
			t.Fatalf("size_token %s: compile: %v", tc.token, err)
		}
		if got := derefString(wire.ark.Size); got != tc.want {
			t.Errorf("size_token %s: size = %q, want %q", tc.token, got, tc.want)
		}
	}
}

func TestTransportImageRawCarriesExtendedFields(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != generateImagesPath {
			t.Errorf("path = %q, want %q", request.URL.Path, generateImagesPath)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		for key, want := range map[string]any{
			"model":               "ep-test",
			"prompt":              "make it transparent",
			"response_format":     "url",
			"layer_decomposition": true,
			"background":          "transparent",
		} {
			if body[key] != want {
				t.Errorf("body[%q] = %#v, want %#v", key, body[key], want)
			}
		}
		if request.Header.Get("Authorization") != "Bearer sk-test" {
			t.Errorf("Authorization = %q, want Bearer sk-test", request.Header.Get("Authorization"))
		}
		writer.Header().Set(arkmodel.ClientRequestHeader, "req-raw")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"model": "ep-test",
			"created": 1,
			"data": [{"url": "https://example.com/out.png", "size": "2048x2048"}],
			"usage": {"generated_images": 1, "output_tokens": 100, "total_tokens": 100}
		}`))
	}))
	defer server.Close()

	enabled := true
	cls := &clients{
		apiKey:     "sk-test",
		baseURL:    server.URL,
		httpClient: server.Client(),
	}
	request := imageTransportRequest("ep-test", "make it transparent")
	// layer_decomposition now rides the SDK request; background still forces
	// the raw body path, which re-marshals that request and adds the field.
	request.ark.LayerDecomposition = &enabled
	request.background = "transparent"
	raw, err := transportImage(cls)(context.Background(), request)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if len(raw.images) != 1 || raw.images[0].url != "https://example.com/out.png" {
		t.Fatalf("raw.images = %#v, want one url image", raw.images)
	}
	if raw.requestID != "req-raw" {
		t.Fatalf("raw.requestID = %q, want req-raw", raw.requestID)
	}
	if raw.outputTokens != 100 || raw.totalTokens != 100 {
		t.Fatalf("usage = %#v, want 100/100", raw)
	}
}

func TestTransportImageSDKRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != generateImagesPath {
			t.Errorf("path = %q, want %q", request.URL.Path, generateImagesPath)
		}
		writer.Header().Set(arkmodel.ClientRequestHeader, "req-sdk")
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"model": "ep-test",
			"created": 1,
			"data": [{"url": "https://example.com/out.png", "size": "2048x2048"}],
			"usage": {"generated_images": 1, "output_tokens": 100, "total_tokens": 100}
		}`))
	}))
	defer server.Close()

	client := arkruntime.NewClientWithApiKey(
		"sk-test",
		arkruntime.WithBaseUrl(server.URL),
		arkruntime.WithHTTPClient(server.Client()),
		arkruntime.WithRetryTimes(0),
	)
	cls := &clients{ark: client}
	raw, err := transportImage(cls)(
		context.Background(),
		imageTransportRequest("ep-test", "a cat"),
	)
	if err != nil {
		t.Fatalf("transport: %v", err)
	}
	if raw.requestID != "req-sdk" {
		t.Fatalf("raw.requestID = %q, want req-sdk", raw.requestID)
	}
}

// TestImageCompileToTransportBody closes the loop the wire used to close: one
// canonical request compiles into the images body the API receives, intent and
// extension knobs included.
func TestImageCompileToTransportBody(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"model": "ep-test",
			"created": 1,
			"data": [{"url": "https://example.com/out.png", "size": "2048x2048"}],
			"usage": {"generated_images": 1, "output_tokens": 100, "total_tokens": 100}
		}`))
	}))
	defer server.Close()

	watermark := false
	request := compileImageRequest(
		[]message.Part{
			message.TextPart{Text: "a red circle"},
			imageReferencePart(t),
		},
		ImageOptions{Watermark: &watermark},
	)
	request.Input.Content.Intent.Image = &inference.ImageIntent{
		Size:         &media.ImageSize{Width: 2048, Height: 2048},
		OutputFormat: media.ImageFormatJPEG,
	}
	compiled, report, err := compileImageWire(t, request)
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}

	client := arkruntime.NewClientWithApiKey(
		"sk-test",
		arkruntime.WithBaseUrl(server.URL),
		arkruntime.WithHTTPClient(server.Client()),
		arkruntime.WithRetryTimes(0),
	)
	if _, err := transportImage(&clients{ark: client})(
		context.Background(),
		compiled,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}

	for key, want := range map[string]any{
		"model":           "ep-test",
		"prompt":          "a red circle",
		"size":            "2048x2048",
		"output_format":   "jpeg",
		"watermark":       false,
		"response_format": "url",
	} {
		if body[key] != want {
			t.Errorf("body[%q] = %#v, want %#v", key, body[key], want)
		}
	}
	references, ok := body["image"].([]any)
	if !ok || len(references) != 1 {
		t.Fatalf("image = %#v, want one reference image", body["image"])
	}
	if references[0] != "https://example.com/input.png" {
		t.Errorf("image[0] = %#v, want the reference url", references[0])
	}
}

func TestDecodeImageMetadata(t *testing.T) {
	raw := imageRaw{
		images:      []rawImage{{url: "https://example.com/out.png"}},
		mediaType:   "image/png",
		requestID:   "req-test",
		inputTokens: 1,
	}
	response, err := decodeImage(context.Background(), raw)
	if err != nil {
		t.Fatalf("decodeImage: %v", err)
	}
	if response.Metadata.RequestID != "req-test" {
		t.Fatalf("metadata request id = %q, want req-test", response.Metadata.RequestID)
	}
}

func TestTransportImageRawErrorClassification(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusBadRequest)
		_, _ = writer.Write([]byte(`{"error":{"code":"InvalidParameter","message":"bad size"}}`))
	}))
	defer server.Close()

	cls := &clients{
		apiKey:     "sk-test",
		baseURL:    server.URL,
		httpClient: server.Client(),
	}
	request := imageTransportRequest("ep-test", "edit")
	request.background = "transparent"
	_, err := transportImage(cls)(context.Background(), request)
	if err == nil {
		t.Fatal("transport error = nil, want validation")
	}
	if !errdefs.IsValidation(err) {
		t.Fatalf("transport error = %v, want errdefs.Validation", err)
	}
}

// TestImageDeliveryLowering pins the response_format knob: the delivery choice
// is the only place the canonical inline/URL intent reaches the request.
func TestImageDeliveryLowering(t *testing.T) {
	cases := []struct {
		delivery media.SourceKind
		want     string
	}{
		{delivery: "", want: "url"},
		{delivery: media.SourceURL, want: "url"},
		{delivery: media.SourceInline, want: "b64_json"},
	}
	for _, tc := range cases {
		t.Run(string(tc.delivery), func(t *testing.T) {
			request := compileImageRequest(
				[]message.Part{message.TextPart{Text: "a cat"}},
				ImageOptions{},
			)
			request.Input.Content.Intent.Image.Delivery = tc.delivery
			compiled, report, err := compileImageWire(t, request)
			if err != nil {
				t.Fatalf("compile: %v; report = %+v", err, report)
			}
			if got := derefString(compiled.ark.ResponseFormat); got != tc.want {
				t.Fatalf("response_format = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestImageTransportFanOut locks the multi-image contract: the endpoint
// returns one image per call, so a count above one repeats the call unless
// grouped generation is on (then one call returns the whole set).
func TestImageTransportFanOut(t *testing.T) {
	cases := []struct {
		name      string
		count     int
		grouped   bool
		wantCalls int
		wantParts int
	}{
		{name: "single", count: 1, wantCalls: 1, wantParts: 1},
		{name: "three calls", count: 3, wantCalls: 3, wantParts: 3},
		{name: "grouped is one call", count: 3, grouped: true, wantCalls: 1, wantParts: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				_ *http.Request,
			) {
				calls++
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{
					"model": "ep-test",
					"created": 1,
					"data": [{"url": "https://example.com/out.png", "size": "1024x1024"}],
					"usage": {"generated_images": 1, "output_tokens": 1, "total_tokens": 1}
				}`))
			}))
			defer server.Close()

			request := imageTransportRequest("ep-test", "a cat")
			request.count = tc.count
			if tc.grouped {
				grouped := arkmodel.SequentialImageGeneration(
					arkmodel.SequentialImageGenerationAuto,
				)
				request.ark.SequentialImageGeneration = &grouped
			}
			raw, err := transportImage(&clients{ark: arkTestClient(t, server)})(
				context.Background(),
				request,
			)
			if err != nil {
				t.Fatalf("transport: %v", err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("provider calls = %d, want %d", calls, tc.wantCalls)
			}
			if len(raw.images) != tc.wantParts {
				t.Fatalf("images = %d, want %d", len(raw.images), tc.wantParts)
			}
		})
	}
}

// TestImageLayerDecompositionRidesTheSDKRequest pins the SDK leaf that the
// pinned request struct used to lack: layer decomposition goes out on the
// typed SDK call, so only background still needs the raw body path.
func TestImageLayerDecompositionRidesTheSDKRequest(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
			"model": "ep-test",
			"created": 1,
			"data": [{"url": "https://example.com/out.png", "size": "2048x2048"}],
			"usage": {"generated_images": 1, "output_tokens": 1, "total_tokens": 1}
		}`))
	}))
	defer server.Close()

	enabled := true
	compiled, report, err := compileImageWire(t, compileImageRequest(
		[]message.Part{imageReferencePart(t)},
		ImageOptions{LayerDecomposition: &enabled},
	))
	if err != nil {
		t.Fatalf("compile: %v; report = %+v", err, report)
	}
	if compiled.background != "" {
		t.Fatal("layer decomposition alone must not need the raw body path")
	}
	if _, err := transportImage(&clients{ark: arkTestClient(t, server)})(
		context.Background(),
		compiled,
	); err != nil {
		t.Fatalf("transport: %v", err)
	}
	if body["layer_decomposition"] != true {
		t.Errorf("layer_decomposition = %#v, want true", body["layer_decomposition"])
	}
}
