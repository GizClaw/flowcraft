package minimax

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/inference/model"
	"github.com/GizClaw/flowcraft/core/inference/protocol"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
)

// benchVideoSpec declares the task-API surface: a video model that accepts text
// and image references.
func benchVideoSpec() ModelSpec {
	return ModelSpec{
		Name: "MiniMax-Hailuo-2.3",
		Kind: "video",
		Capabilities: model.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText, message.PartImage},
			Outputs: []message.PartKind{message.PartVideo},
		},
	}
}

func benchImagePart(tb testing.TB, size int) message.Part {
	tb.Helper()
	source, err := media.NewImageBytes(make([]byte, size), "image/png")
	if err != nil {
		tb.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

// BenchmarkCompileVideo is the task-API request build: prompt assembly plus the
// reference-image rendering that turns inline bytes into a data URI. minimax is
// the one driver that already encodes media in place (AppendEncode), so this is
// the regression guard for that path.
func BenchmarkCompileVideo(b *testing.B) {
	compile := compileVideo(benchVideoSpec(), "MiniMax-Hailuo-2.3")
	request := inference.GenerateRequest{
		Model:  model.ModelRef{ID: model.ModelID{Provider: ProviderID, Name: "MiniMax-Hailuo-2.3"}},
		Output: inference.OutputVideo,
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: message.Content{Parts: []message.Part{
				message.TextPart{Text: "A slow dolly shot through a neon-lit alley."},
				benchImagePart(b, 512<<10),
			}},
		},
	}
	b.ReportAllocs()
	b.SetBytes(512 << 10)
	for i := 0; i < b.N; i++ {
		if _, err := compile(context.Background(), request, protocol.ShapeUnary); err != nil {
			b.Fatal(err)
		}
	}
}
