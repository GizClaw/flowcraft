package minimax

// The credentialed lane: these tests call the real MiniMax API and skip
// themselves when no key is present.
//
//	MINIMAX_API_KEY=... go test ./minimax/ -run TestLive -v
//	MINIMAX_API_KEY=... LIVE_SLOW=1 go test ./minimax/ -run TestLive -v   # + video
//
// Model and voice ids are per account, so each is taken from the environment
// with the documented default behind it.

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/model"
)

func liveSettings(t *testing.T, spec ModelSpec) inference.Provider {
	t.Helper()
	key := os.Getenv("MINIMAX_API_KEY")
	if key == "" {
		t.Skip("MINIMAX_API_KEY is not set: the credentialed lane is opt-in")
	}
	settings := Settings{APIKey: key, Models: []ModelSpec{spec}}
	if base := os.Getenv("MINIMAX_BASE_URL"); base != "" {
		settings.BaseURL = base
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

// TestLiveSpeech is the cheapest real call: synthesis is character-billed, so one
// short line proves the request shape, the envelope and the audio decode.
func TestLiveSpeech(t *testing.T) {
	name := liveModel(t, "MINIMAX_TTS_MODEL", "speech-2.6-hd")
	provider := liveSettings(t, ModelSpec{
		Name: name, Kind: "tts",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
			Modes:  []model.Mode{model.ModeAudio},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	response, err := provider.Generate(ctx, inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("live check"),
		},
		Output: inference.OutputAudio,
		// The typed extension is the speech surface's own option block: the voice
		// is required, and this is how one is attached.
		Extensions: inference.Extensions{
			&TTSOptions{Voice: liveModel(t, "MINIMAX_VOICE", "female-shaonv")},
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

// TestLiveVideo is the slow, billed path: an asynchronous render plus polling.
// It is opt-in on top of the key.
func TestLiveVideo(t *testing.T) {
	if os.Getenv("LIVE_SLOW") == "" {
		t.Skip("LIVE_SLOW is not set: a video render takes minutes and is billed")
	}
	name := liveModel(t, "MINIMAX_VIDEO_MODEL", "MiniMax-H3")
	provider := liveSettings(t, ModelSpec{
		Name: name, Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs: []message.PartKind{message.PartText},
			Modes:  []model.Mode{model.ModeVideo},
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	response, err := provider.Generate(ctx, inference.GenerateRequest{
		Model: model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: name}},
		Input: inference.GenerateInput{
			Role:    inference.InputRoleUser,
			Content: message.NewTextContent("a calm lake at dawn"),
		},
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
