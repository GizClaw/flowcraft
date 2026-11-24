package dashscope

// The credentialed lane: these tests call the real DashScope API and skip
// themselves when no key is present.
//
//	DASHSCOPE_API_KEY=... go test ./dashscope/ -run TestLive -v
//	DASHSCOPE_API_KEY=... LIVE_SLOW=1 go test ./dashscope/ -run TestLive -v   # + media tasks
//
// Model names are per region and per account, so each is taken from the
// environment with the documented default behind it.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/model"
)

func liveSettings(t *testing.T, spec ModelSpec) inference.Provider {
	t.Helper()
	key := os.Getenv("DASHSCOPE_API_KEY")
	if key == "" {
		t.Skip("DASHSCOPE_API_KEY is not set: the credentialed lane is opt-in")
	}
	settings := Spec{APIKey: key, Models: []ModelSpec{spec}}
	if base := os.Getenv("DASHSCOPE_BASE_URL"); base != "" {
		settings.BaseURL = base
	}
	if workspace := os.Getenv("DASHSCOPE_WORKSPACE_ID"); workspace != "" {
		settings.WorkspaceID = workspace
	}
	provider, err := New(settings)
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

func liveSpec(kind, name string, inputs, outputs []message.PartKind) ModelSpec {
	capabilities := model.ModelCapabilities{Inputs: inputs, Outputs: outputs}
	if kind == "embed" {
		capabilities.EmbeddingDimensions = 1024
	}
	spec := ModelSpec{Name: name, Kind: kind, Capabilities: capabilities}
	if kind == "tts" {
		// The speech surface has no format control, so the declaration states the
		// encoding it serves. The Qwen-TTS family answers WAV around raw PCM, so
		// the declaration names the stream shape with it.
		spec.TTS = TTSParams{Encoding: media.AudioEncodingPCM16, SampleRateHz: 24000, Channels: 1}
	}
	return spec
}

// TestLiveEmbed is the cheapest real call: one text in, one vector out.
func TestLiveEmbed(t *testing.T) {
	name := liveModel(t, "DASHSCOPE_EMBED_MODEL", "text-embedding-v4")
	provider := liveSettings(t, liveSpec("embed", name,
		[]message.PartKind{message.PartText}, nil))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	response, err := provider.Embedder.Embed(ctx, inference.EmbedRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input: []message.Content{message.NewTextContent("flowcraft live check")},
	})
	if err != nil {
		t.Fatalf("Embed: %v\n  cause: %v", err, errors.Unwrap(err))
	}
	if len(response.Vectors) != 1 || len(response.Vectors[0]) == 0 {
		t.Fatalf("vectors = %d, want one non-empty vector", len(response.Vectors))
	}
	t.Logf("embed %s: %d dims", name, len(response.Vectors[0]))
}

// TestLiveSpeech proves the synthesis surface: one short line, audio back.
func TestLiveSpeech(t *testing.T) {
	name := liveModel(t, "DASHSCOPE_TTS_MODEL", "qwen3-tts-flash")
	provider := liveSettings(t, liveSpec("tts", name,
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartAudio}))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	response, err := provider.Generate(ctx, inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("live check"),
		},
		Output: inference.OutputAudio,
		Extensions: inference.Extensions{
			&TTSOptions{Voice: liveModel(t, "DASHSCOPE_VOICE", "Cherry")},
		},
	})
	if err != nil {
		t.Fatalf("Generate(speech): %v\n  cause: %v", err, errors.Unwrap(err))
	}
	if len(response.Message.Content.Parts) == 0 {
		t.Fatal("no audio part returned")
	}
	t.Logf("speech %s: %d part(s)", name, len(response.Message.Content.Parts))
}

// TestLiveImage is the asynchronous task path: create, poll, download.
func TestLiveImage(t *testing.T) {
	if os.Getenv("LIVE_SLOW") == "" {
		t.Skip("LIVE_SLOW is not set: a media task is billed and slow")
	}
	name := liveModel(t, "DASHSCOPE_IMAGE_MODEL", "wanx2.1-t2i-turbo")
	provider := liveSettings(t, liveSpec("image", name,
		[]message.PartKind{message.PartText}, []message.PartKind{message.PartImage}))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	response, err := provider.Generate(ctx, inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("a single red circle on white"),
		},
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
