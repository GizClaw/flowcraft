package openai

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/message"
)

// collect drains a stream and returns the text and the assembled response.
func collect(t *testing.T, stream inference.Stream) (string, inference.GenerateResponse) {
	t.Helper()
	var text strings.Builder
	for {
		event, err := stream.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if event.Kind == inference.StreamText {
			text.WriteString(event.Text)
		}
	}
	response, err := stream.Result()
	if err != nil {
		t.Fatalf("result: %v", err)
	}
	return text.String(), response
}

func TestLiveStream(t *testing.T) {
	key := os.Getenv("OPENAI_API_KEY")
	if key == "" {
		t.Skip("OPENAI_API_KEY not set")
	}
	for _, api := range []API{APIChat, APIResponses} {
		t.Run(string(api), func(t *testing.T) {
			provider, err := New(Settings{API: api, APIKey: key})
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			request := inference.GenerateRequest{
				Model: model.ModelRef{
					ID:      model.ModelID{Provider: ProviderID, Name: "gpt-4o-mini"},
					Profile: "default",
				},
				Input: inference.GenerateInput{
					Role:    inference.InputRoleUser,
					Content: message.NewTextContent("Count from 1 to 5, separated by spaces, nothing else."),
				},
			}
			stream, err := provider.GenerateStream(context.Background(), request)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = stream.Close() }()
			text, response := collect(t, stream)
			t.Logf("%s -> %q (finish=%s, in=%d out=%d)",
				api, text, response.FinishReason,
				response.Usage.InputTokens, response.Usage.OutputTokens)
			if text == "" {
				t.Fatal("empty streamed answer")
			}
		})
	}
}
