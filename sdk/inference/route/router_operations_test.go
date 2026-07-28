package route

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/inference"
	"github.com/GizClaw/flowcraft/sdk/inference/media"
)

type embedSelectorFunc func(context.Context, inference.EmbedRequest) (Decision, error)

func (f embedSelectorFunc) SelectEmbed(
	ctx context.Context,
	request inference.EmbedRequest,
) (Decision, error) {
	return f(ctx, request)
}

type transcriptionSelectorFunc func(
	context.Context,
	inference.TranscriptionRequest,
) (Decision, error)

func (f transcriptionSelectorFunc) SelectTranscription(
	ctx context.Context,
	request inference.TranscriptionRequest,
) (Decision, error) {
	return f(ctx, request)
}

type realtimeSelectorFunc func(context.Context, inference.RealtimeConfig) (Decision, error)

func (f realtimeSelectorFunc) SelectRealtime(
	ctx context.Context,
	config inference.RealtimeConfig,
) (Decision, error) {
	return f(ctx, config)
}

func TestRouterExposesSelectorsForCurrentOperations(t *testing.T) {
	runtime := newGenerateRouteRuntime(t, map[string]generateRouteBehavior{
		"model": {},
	})
	invalid := Decision{Operation: inference.OperationGenerate}

	t.Run("embed", func(t *testing.T) {
		router, err := New(runtime, Selectors{Embed: embedSelectorFunc(func(
			context.Context,
			inference.EmbedRequest,
		) (Decision, error) {
			return invalid, nil
		})})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _, err = router.ExplainEmbed(context.Background(), inference.EmbedRequest{
			Items: []inference.EmbedItem{{Content: inference.Content{
				Parts: []inference.Part{inference.TextPart{Text: "hello"}},
			}}},
		})
		assertSelectorContractViolation(t, err)
	})

	t.Run("transcription", func(t *testing.T) {
		audio, err := media.NewAudioBytes([]byte("audio"), "audio/wav")
		if err != nil {
			t.Fatalf("NewAudioBytes: %v", err)
		}
		router, err := New(runtime, Selectors{
			Transcription: transcriptionSelectorFunc(func(
				context.Context,
				inference.TranscriptionRequest,
			) (Decision, error) {
				return invalid, nil
			}),
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _, err = router.ExplainTranscription(
			context.Background(),
			inference.TranscriptionRequest{Audio: audio},
		)
		assertSelectorContractViolation(t, err)
	})

	t.Run("realtime", func(t *testing.T) {
		router, err := New(runtime, Selectors{Realtime: realtimeSelectorFunc(func(
			context.Context,
			inference.RealtimeConfig,
		) (Decision, error) {
			return invalid, nil
		})})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		_, _, err = router.ExplainRealtime(
			context.Background(),
			inference.RealtimeConfig{Modalities: []inference.Modality{
				inference.ModalityText,
			}},
		)
		assertSelectorContractViolation(t, err)
	})
}

func assertSelectorContractViolation(t *testing.T, err error) {
	t.Helper()
	if !IsKind(err, SelectorContractViolation) {
		t.Fatalf("error = %v, want SelectorContractViolation", err)
	}
}
