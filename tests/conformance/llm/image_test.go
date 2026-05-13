package llm_test

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/llm"
	"github.com/GizClaw/flowcraft/sdk/model"

	// Trigger init() self-registration for the image-generation
	// adapters. They live under separate provider keys so the chat
	// imports above do NOT pull them in implicitly.
	_ "github.com/GizClaw/flowcraft/sdkx/llm/bytedance/image"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/minimax/image"
	_ "github.com/GizClaw/flowcraft/sdkx/llm/qwen/image"
)

// imageProviders enumerates the image-generation adapters under
// test. Each entry self-skips when its env var is unset so a
// credential-less run is a no-op (matches the chat-conformance
// pattern). Provider keys here are deliberately distinct from the
// chat provider keys ("minimax", "bytedance", "qwen") since image
// catalogs are registered under separate keys to keep capability
// matrices clean.
var imageProviders = []providerSpec{
	{Provider: "minimax-image", Env: "FLOWCRAFT_TEST_MINIMAX_IMAGE"},
	{Provider: "bytedance-image", Env: "FLOWCRAFT_TEST_BYTEDANCE_IMAGE"},
	{Provider: "qwen-image", Env: "FLOWCRAFT_TEST_QWEN_IMAGE"},
}

// countImageParts returns the number of [model.PartImage] parts in
// msg, regardless of whether they're URL- or base64-backed.
func countImageParts(msg llm.Message) int {
	n := 0
	for _, p := range msg.Parts {
		if p.Type == llm.PartImage {
			n++
		}
	}
	return n
}

// firstImageDescriptor returns a short stable descriptor of the
// first image part, useful for log output without dumping the full
// payload.
func firstImageDescriptor(msg llm.Message) string {
	for _, p := range msg.Parts {
		if p.Type != llm.PartImage || p.Image == nil {
			continue
		}
		switch {
		case p.Image.URL != "":
			return "url=" + p.Image.URL
		case p.Image.Base64 != "":
			d := p.Image.Base64
			if len(d) > 32 {
				d = d[:32] + "…"
			}
			return "base64=" + d
		}
	}
	return "<no image>"
}

// maybeSaveImages writes every [model.PartImage] in msg to
// tests/conformance/llm/_out/ when SAVE_GENERATED_IMAGES is truthy.
// Unset / "0" / "false" → no-op (default for CI and credential-less
// runs). The directory is created on demand and listed in
// .gitignore so accidental commits are prevented.
//
// Filenames are stable enough for human inspection but unique per
// invocation: "{provider}_{test}_{nanos}_{idx}.{ext}". Failure to
// download is logged with t.Errorf — the test still passes if the
// API contract held; only the offline review fails.
func maybeSaveImages(t *testing.T, provider string, msg llm.Message) {
	t.Helper()
	if !envTruthy(os.Getenv("SAVE_GENERATED_IMAGES")) {
		return
	}
	dir := outputDir(t)
	if dir == "" {
		return
	}
	stamp := time.Now().UnixNano()
	idx := 0
	for _, p := range msg.Parts {
		if p.Type != llm.PartImage || p.Image == nil {
			continue
		}
		ext, data, err := fetchImageBytes(p.Image)
		if err != nil {
			t.Errorf("fetch image bytes for save: %v", err)
			continue
		}
		name := fmt.Sprintf("%s_%s_%d_%d.%s",
			sanitizeForFilename(provider),
			sanitizeForFilename(t.Name()),
			stamp, idx, ext)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Errorf("write %s: %v", path, err)
			continue
		}
		t.Logf("saved -> %s (%d bytes)", path, len(data))
		idx++
	}
}

func fetchImageBytes(ref *model.MediaRef) (ext string, data []byte, err error) {
	switch {
	case ref.URL != "":
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Get(ref.URL)
		if err != nil {
			return "", nil, fmt.Errorf("GET %s: %w", ref.URL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return "", nil, fmt.Errorf("GET %s: status %d", ref.URL, resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", nil, err
		}
		return extFromMediaTypeOrURL(ref.MediaType, ref.URL), body, nil
	case ref.Base64 != "":
		body, err := base64.StdEncoding.DecodeString(ref.Base64)
		if err != nil {
			return "", nil, fmt.Errorf("decode base64: %w", err)
		}
		return extFromMediaTypeOrURL(ref.MediaType, ""), body, nil
	default:
		return "", nil, fmt.Errorf("MediaRef has neither URL nor Base64")
	}
}

func extFromMediaTypeOrURL(mediaType, url string) string {
	switch mediaType {
	case "image/png":
		return "png"
	case "image/jpeg", "image/jpg":
		return "jpg"
	case "image/webp":
		return "webp"
	}
	if i := strings.IndexByte(url, '?'); i >= 0 {
		url = url[:i]
	}
	switch e := strings.ToLower(filepath.Ext(url)); e {
	case ".png", ".jpg", ".jpeg", ".webp":
		return strings.TrimPrefix(e, ".")
	}
	return "bin"
}

// outputDir returns the absolute path of the per-package output
// directory, creating it on demand. It anchors at the test's CWD
// (the conformance/llm/ package directory under `go test`).
func outputDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Errorf("getwd: %v", err)
		return ""
	}
	dir := filepath.Join(wd, "_out")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("mkdir %s: %v", dir, err)
		return ""
	}
	return dir
}

func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	}
	return true
}

func sanitizeForFilename(s string) string {
	r := strings.NewReplacer("/", "_", " ", "_", ":", "_", string(filepath.Separator), "_")
	return r.Replace(s)
}

// ---------------------------------------------------------------------------
// Scenario 1: basic text-to-image — minimal smoke test for every adapter
// ---------------------------------------------------------------------------

func TestImageProviders_BasicTextToImage(t *testing.T) {
	for _, spec := range imageProviders {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "A photorealistic ginger kitten sitting on a wooden floor, soft natural light."),
			}
			resp, _, err := provider.Generate(ctx, msgs)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			if got := countImageParts(resp); got == 0 {
				t.Fatalf("expected at least 1 image part, got 0 (parts=%+v)", resp.Parts)
			}
			t.Logf("first=%s images=%d", firstImageDescriptor(resp), countImageParts(resp))
			maybeSaveImages(t, spec.Provider, resp)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 2: explicit Width/Height — every adapter must accept and route
// the dimensions to its provider-native field. Adapters internally translate
// to "WxH" (MiniMax/Seedream) or "W*H" (Qwen).
//
// Each family has very different valid ranges, so we use per-provider
// sizes that are known to be accepted:
//
//   - minimax-image:   image-01 accepts 256–1536 per side (multiple of 8).
//   - bytedance-image: Seedream 5.0 requires ≥1920² total pixels.
//   - qwen-image:      2.0 series accepts 512–2048 total pixel range.
// ---------------------------------------------------------------------------

func TestImageProviders_ExplicitDimensions(t *testing.T) {
	type sizeCase struct {
		provider providerSpec
		w, h     int
	}
	cases := []sizeCase{
		{providerSpec{Provider: "minimax-image", Env: "FLOWCRAFT_TEST_MINIMAX_IMAGE"}, 1024, 1024},
		{providerSpec{Provider: "bytedance-image", Env: "FLOWCRAFT_TEST_BYTEDANCE_IMAGE"}, 2048, 2048},
		{providerSpec{Provider: "qwen-image", Env: "FLOWCRAFT_TEST_QWEN_IMAGE"}, 1024, 1024},
	}
	for _, tc := range cases {
		t.Run(tc.provider.Provider, func(t *testing.T) {
			provider := createProvider(t, tc.provider)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "A simple geometric pattern in pastel colors."),
			}
			resp, _, err := provider.Generate(
				ctx, msgs,
				llm.WithImageGen(llm.ImageGenOptions{Width: tc.w, Height: tc.h}),
			)
			if err != nil {
				t.Fatalf("Generate error (%dx%d): %v", tc.w, tc.h, err)
			}
			if got := countImageParts(resp); got == 0 {
				t.Fatalf("expected at least 1 image part, got 0")
			}
			t.Logf("size=%dx%d first=%s", tc.w, tc.h, firstImageDescriptor(resp))
			maybeSaveImages(t, tc.provider.Provider, resp)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 3: streaming via NewOneChunkStream — every adapter wraps its
// synchronous response in a single-chunk stream. Validates the
// llm.StreamMessage contract: Next() once, Err() == nil, Message() carries
// the image parts.
// ---------------------------------------------------------------------------

func TestImageProviders_Streaming(t *testing.T) {
	for _, spec := range imageProviders {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()

			msgs := []llm.Message{
				llm.NewTextMessage(llm.RoleUser, "A red apple on a white background."),
			}
			stream, err := provider.GenerateStream(ctx, msgs)
			if err != nil {
				t.Fatalf("GenerateStream error: %v", err)
			}
			defer stream.Close()
			chunks := 0
			for stream.Next() {
				chunks++
			}
			if err := stream.Err(); err != nil {
				t.Fatalf("stream error: %v", err)
			}
			final := stream.Message()
			if got := countImageParts(final); got == 0 {
				t.Fatalf("expected at least 1 image part in final message, got 0")
			}
			t.Logf("chunks=%d first=%s", chunks, firstImageDescriptor(final))
			maybeSaveImages(t, spec.Provider, final)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 4: image-to-image with a reference URL.
//
// Only minimax-image and bytedance-image support reference-image input on
// their generation endpoints. qwen-image rejects PartImage at the adapter
// level (image editing is on the separate qwen-image-edit endpoint).
// ---------------------------------------------------------------------------

func TestImageProviders_ImageToImage(t *testing.T) {
	// Reference image hosted on Aliyun help-static CDN. We deliberately
	// avoid Wikipedia / GitHub-raw URLs because the upstream image
	// generation servers (MiniMax in Shanghai, Volcengine Ark in
	// Beijing) often cannot reach foreign CDNs and surface the result
	// as cryptic "invalid params, img_url" / "Timeout while
	// downloading url" errors. The chosen URL is one of Qwen's own
	// example images and is reachable from both regions.
	const refImageURL = "https://help-static-aliyun-doc.aliyuncs.com/assets/img/zh-CN/0552803771/p1005422.png"

	specs := []providerSpec{
		{Provider: "minimax-image", Env: "FLOWCRAFT_TEST_MINIMAX_IMAGE"},
		{Provider: "bytedance-image", Env: "FLOWCRAFT_TEST_BYTEDANCE_IMAGE"},
	}
	for _, spec := range specs {
		t.Run(spec.Provider, func(t *testing.T) {
			provider := createProvider(t, spec)
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
			defer cancel()

			msgs := []llm.Message{
				{
					Role: llm.RoleUser,
					Parts: []model.Part{
						{Type: llm.PartText, Text: "Re-imagine this cat in an oil-painting style."},
						{Type: llm.PartImage, Image: &model.MediaRef{URL: refImageURL, MediaType: "image/jpeg"}},
					},
				},
			}
			resp, _, err := provider.Generate(ctx, msgs)
			if err != nil {
				t.Fatalf("Generate error: %v", err)
			}
			if got := countImageParts(resp); got == 0 {
				t.Fatalf("expected at least 1 image part, got 0")
			}
			t.Logf("first=%s", firstImageDescriptor(resp))
			maybeSaveImages(t, spec.Provider, resp)
		})
	}
}

// ---------------------------------------------------------------------------
// Scenario 5: qwen-image must REJECT image inputs (this endpoint is
// text-to-image only; image editing lives on qwen-image-edit).
// ---------------------------------------------------------------------------

func TestImageProviders_QwenRejectsImageInput(t *testing.T) {
	spec := providerSpec{Provider: "qwen-image", Env: "FLOWCRAFT_TEST_QWEN_IMAGE"}
	provider := createProvider(t, spec)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msgs := []llm.Message{
		{
			Role: llm.RoleUser,
			Parts: []model.Part{
				{Type: llm.PartText, Text: "Edit this please."},
				{Type: llm.PartImage, Image: &model.MediaRef{URL: "https://example.com/whatever.png"}},
			},
		},
	}
	_, _, err := provider.Generate(ctx, msgs)
	if err == nil {
		t.Fatal("expected validation error for image input on qwen-image, got nil")
	}
	// The error message should mention either qwen-image-edit or
	// text-to-image so callers can tell why the request was refused.
	msg := err.Error()
	if !strings.Contains(msg, "text-to-image") && !strings.Contains(msg, "edit") {
		t.Errorf("error %q does not mention t2i-only constraint", msg)
	}
	t.Logf("rejected as expected: %v", err)
}
