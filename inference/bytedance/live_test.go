package bytedance

// The credentialed lane: these tests call the real Ark API and skip themselves
// when no key is present, the way `core/inference/inferencetest` does.
//
// They exist because nothing else can answer the question the fixtures cannot:
// does the vendor accept the JSON this driver builds? A recorded request proves
// the bytes are unchanged; only a live call proves they are right.
//
//	ARK_API_KEY=... go test ./bytedance/ -run TestLive -v
//	ARK_API_KEY=... LIVE_SLOW=1 go test ./bytedance/ -run TestLive -v   # + video
//
// The model ids are endpoint ids, which differ per account; each test takes one
// from the environment and falls back to a default that the account may rename.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
)

// liveProvider builds a driver for one model, skipping when the lane is not
// configured.
func liveProvider(t *testing.T, kind, modelName string, inputs []message.PartKind, modes []model.Mode) inference.Provider {
	t.Helper()
	key := os.Getenv("ARK_API_KEY")
	if key == "" {
		t.Skip("ARK_API_KEY is not set: the credentialed lane is opt-in")
	}
	capabilities := model.ModelCapabilities{Inputs: inputs, Modes: modes}
	if kind == "embed" {
		capabilities.EmbeddingDimensions = 1024
	}
	provider, err := New(Settings{
		APIKey: key,
		Models: []ModelSpec{{
			Name:         modelName,
			Kind:         kind,
			Capabilities: capabilities,
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return provider
}

func liveModel(t *testing.T, env, fallback string) string {
	t.Helper()
	if value := os.Getenv(env); value != "" {
		return value
	}
	return fallback
}

// TestLiveEmbed is the cheapest call that proves authentication, the request
// shape and the response decode end to end.
func TestLiveEmbed(t *testing.T) {
	// The account's embedding model is a vision one: declaring an image input is
	// what routes the request to the multimodal endpoint it is served at.
	name := liveModel(t, "ARK_EMBED_MODEL", "doubao-embedding-vision-251215")
	provider := liveProvider(t, "embed", name,
		[]message.PartKind{message.PartText, message.PartImage}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	response, err := provider.Embedder.Embed(ctx, inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input: []message.Content{{Parts: []message.Part{
			message.TextPart{Text: "flowcraft live check"},
			message.ImagePart{Source: liveImageSource(t)},
		}}},
	})
	if err != nil {
		t.Fatalf("Embed: %v\n  cause: %v", err, errors.Unwrap(err))
	}
	if len(response.Vectors) != 1 || len(response.Vectors[0]) == 0 {
		t.Fatalf("vectors = %d, want one non-empty vector", len(response.Vectors))
	}
	t.Logf("embed %s: %d dims", name, len(response.Vectors[0]))
}

// TestLiveImage proves the create-and-download path on the cheapest image model
// the account offers.
func TestLiveImage(t *testing.T) {
	name := liveModel(t, "ARK_IMAGE_MODEL", "doubao-seedream-4-0-20260415")
	provider := liveProvider(t, "image", name,
		[]message.PartKind{message.PartText}, []model.Mode{model.ModeImage})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	response, err := provider.Generate(ctx, inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("a single red circle on white")},
		Output: inference.OutputImage,
	})
	if err != nil {
		t.Fatalf("Generate(image): %v\n  cause: %v", err, errors.Unwrap(err))
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("no image part returned")
	}
	t.Logf("image %s: %d part(s)", name, len(response.Message.Content.Parts))
}

// TestLiveVideo is the slow, billed path: an asynchronous task plus polling. It
// is opt-in on top of the key, because a render takes minutes.
func TestLiveVideo(t *testing.T) {
	if os.Getenv("LIVE_SLOW") == "" {
		t.Skip("LIVE_SLOW is not set: a video render takes minutes and is billed")
	}
	name := liveModel(t, "ARK_VIDEO_MODEL", "doubao-seedance-2-0-mini-260615")
	provider := liveProvider(t, "video", name,
		[]message.PartKind{message.PartText}, []model.Mode{model.ModeVideo})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	response, err := provider.Generate(ctx, inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input:  inference.GenerateInput{Role: inference.InputRoleUser, Content: message.NewTextContent("a calm lake at dawn")},
		Output: inference.OutputVideo,
	})
	if err != nil {
		t.Fatalf("Generate(video): %v\n  cause: %v", err, errors.Unwrap(err))
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("no video part returned")
	}
	t.Logf("video %s: %d part(s)", name, len(response.Message.Content.Parts))
}

// liveImageSource is a 16x16 PNG: above the vendor's 14-pixel floor, small
// enough to inline, and built here so the fixture has no bytes to explain.
func liveImageSource(t *testing.T) media.ImageSource {
	t.Helper()
	canvas := image.NewRGBA(image.Rect(0, 0, 16, 16))
	for x := 0; x < 16; x++ {
		for y := 0; y < 16; y++ {
			canvas.Set(x, y, color.RGBA{R: 200, G: 40, B: 40, A: 255})
		}
	}
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, canvas); err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	source, err := media.NewImageBytes(encoded.Bytes(), "image/png")
	if err != nil {
		t.Fatalf("image source: %v", err)
	}
	return source
}
