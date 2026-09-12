package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool"
)

func longTool(n int) tool.Tool {
	return tool.TextTool(message.ToolDefinition{Name: "long"},
		func(_ context.Context, _ string) (string, error) {
			return strings.Repeat("x", n), nil
		})
}

func TestResultLimiter_TruncatesWithMarker(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(1000)), ResultLimiter(100))
	res := exec.Execute(context.Background(), call("long"))
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content.Text())
	}
	if len([]rune(res.Content.Text())) != 100 {
		t.Errorf("limited content length = %d, want 100", len([]rune(res.Content.Text())))
	}
	if !strings.HasSuffix(res.Content.Text(), DefaultResultMarker) {
		t.Errorf("content %q does not end with default marker %q", res.Content.Text(), DefaultResultMarker)
	}
}

func TestResultLimiter_UnderLimitUntouched(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(50)), ResultLimiter(100))
	res := exec.Execute(context.Background(), call("long"))
	if res.Content.Text() != strings.Repeat("x", 50) {
		t.Errorf("content = %q, want the full 50 runes", res.Content.Text())
	}
}

func TestResultLimiter_TruncatesErrorResults(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(tool.TextTool(
		message.ToolDefinition{Name: "boom"},
		func(_ context.Context, _ string) (string, error) {
			return "", &errLike{}
		})), ResultLimiter(20))
	res := exec.Execute(context.Background(), call("boom"))
	if !res.IsError {
		t.Fatal("expected IsError to survive truncation")
	}
	if len([]rune(res.Content.Text())) != 20 {
		t.Errorf("limited error length = %d, want 20", len([]rune(res.Content.Text())))
	}
}

type errLike struct{}

func (e *errLike) Error() string { return strings.Repeat("y", 500) }

func TestResultLimiter_CustomMarker(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(1000)),
		ResultLimiter(64, WithResultMarker("[cut here]")))
	res := exec.Execute(context.Background(), call("long"))
	if !strings.HasSuffix(res.Content.Text(), "[cut here]") {
		t.Errorf("content %q does not end with custom marker", res.Content.Text())
	}
}

func TestResultLimiter_TrimsMarkerToFitTinyLimit(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(1000)),
		ResultLimiter(3, WithResultMarker("MARK")))
	res := exec.Execute(context.Background(), call("long"))
	if res.Content.Text() != "MAR" {
		t.Errorf("content = %q, want marker trimmed to 3 runes", res.Content.Text())
	}
}

func TestResultLimiter_DoesNotSplitRunes(t *testing.T) {
	content := strings.Repeat("你", 100)
	exec := tool.NewExecutor(catalogWith(tool.TextTool(
		message.ToolDefinition{Name: "cjk"},
		func(_ context.Context, _ string) (string, error) { return content, nil })),
		ResultLimiter(40))
	res := exec.Execute(context.Background(), call("cjk"))
	if !strings.HasSuffix(res.Content.Text(), DefaultResultMarker) {
		t.Fatalf("content %q does not end with marker", res.Content.Text())
	}
	if len([]rune(res.Content.Text())) != 40 {
		t.Errorf("limited content length = %d, want 40 runes", len([]rune(res.Content.Text())))
	}
}

func TestResultLimiter_TruncatesTextAndKeepsMedia(t *testing.T) {
	source, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	mediaTool := tool.FuncTool(message.ToolDefinition{Name: "shot"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: strings.Repeat("x", 500)},
				message.ImagePart{Source: source},
				message.TextPart{Text: "tail"},
			}}, nil
		})

	exec := tool.NewExecutor(catalogWith(mediaTool), ResultLimiter(50))
	res := exec.Execute(context.Background(), call("shot"))
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content.Text())
	}
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (limited text + surviving image)", len(res.Content.Parts))
	}
	text, ok := res.Content.Parts[0].(message.TextPart)
	if !ok {
		t.Fatalf("part 0 = %T, want message.TextPart", res.Content.Parts[0])
	}
	if !strings.HasSuffix(text.Text, DefaultResultMarker) {
		t.Fatalf("text %q does not end with the truncation marker", text.Text)
	}
	if got := len([]rune(text.Text)); got != 50 {
		t.Fatalf("text budget = %d runes, want 50", got)
	}
	if _, ok := res.Content.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("part 1 = %T, want the untouched image part", res.Content.Parts[1])
	}
}

func TestResultLimiter_MarkerSurvivesExhaustedBudget(t *testing.T) {
	source, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	mediaTool := tool.FuncTool(message.ToolDefinition{Name: "shot"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.ImagePart{Source: source},
				message.TextPart{Text: strings.Repeat("x", 500)},
			}}, nil
		})

	exec := tool.NewExecutor(catalogWith(mediaTool), ResultLimiter(5))
	res := exec.Execute(context.Background(), call("shot"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (image + marker)", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.ImagePart); !ok {
		t.Fatalf("part 0 = %T, want the untouched image part", res.Content.Parts[0])
	}
	marker, ok := res.Content.Parts[1].(message.TextPart)
	if !ok {
		t.Fatalf("part 1 = %T, want the truncation marker", res.Content.Parts[1])
	}
	if got := len([]rune(marker.Text)); got != 5 {
		t.Fatalf("marker = %q (%d runes), want the marker trimmed to 5", marker.Text, got)
	}
}

func TestResultLimiter_MediaAloneNeverTriggersMarker(t *testing.T) {
	source, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	mediaTool := tool.FuncTool(message.ToolDefinition{Name: "shot"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: "caption"},
				message.ImagePart{Source: source},
			}}, nil
		})

	// The rendered form is far longer than the limit, but every text part
	// fits, so nothing is dropped and the result stays intact.
	exec := tool.NewExecutor(catalogWith(mediaTool), ResultLimiter(30))
	res := exec.Execute(context.Background(), call("shot"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (nothing to truncate)", len(res.Content.Parts))
	}
	if text, ok := res.Content.Parts[0].(message.TextPart); !ok || text.Text != "caption" {
		t.Fatalf("part 0 = %#v, want the untouched caption", res.Content.Parts[0])
	}
}

func TestResultLimiter_InvalidMaxPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for non-positive max")
		}
	}()
	ResultLimiter(0)
}

// dataTool returns a tool whose result is one structured data part
// carrying payload.
func dataTool(name, payload string) tool.Tool {
	return tool.FuncTool(message.ToolDefinition{Name: name},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.NewJSONContent([]byte(payload))
		})
}

func TestResultLimiter_DropsPartOverByteBudget(t *testing.T) {
	payload := `{"blob":"` + strings.Repeat("x", 2048) + `"}`
	exec := tool.NewExecutor(catalogWith(dataTool("data", payload)),
		ResultLimiter(1000, WithResultPartBudget(256)))

	res := exec.Execute(context.Background(), call("data"))
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content.Text())
	}
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the marker alone", len(res.Content.Parts))
	}
	if got := res.Content.Text(); !strings.HasSuffix(got, DefaultResultMarker) {
		t.Fatalf("content = %q, want the truncation marker", got)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestResultLimiter_KeepsPartWithinByteBudget(t *testing.T) {
	payload := `{"answer":42}`
	exec := tool.NewExecutor(catalogWith(dataTool("data", payload)),
		ResultLimiter(1000, WithResultPartBudget(4096)))

	res := exec.Execute(context.Background(), call("data"))
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the untouched data part", len(res.Content.Parts))
	}
	if data, ok := res.Content.Parts[0].(message.DataPart); !ok || string(data.Value) != payload {
		t.Fatalf("part = %#v, want the untouched data part", res.Content.Parts[0])
	}
}

func TestResultLimiter_SpendsPartBudgetAcrossParts(t *testing.T) {
	one, err := message.NewJSONContent([]byte(`{"blob":"` + strings.Repeat("y", 128) + `"}`))
	if err != nil {
		t.Fatalf("NewJSONContent: %v", err)
	}
	encoded, err := message.MarshalPart(one.Parts[0])
	if err != nil {
		t.Fatalf("MarshalPart: %v", err)
	}
	two := tool.FuncTool(message.ToolDefinition{Name: "two"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{one.Parts[0], one.Parts[0]}}, nil
		})

	// The budget covers exactly one part, so the second is dropped.
	exec := tool.NewExecutor(catalogWith(two),
		ResultLimiter(1000, WithResultPartBudget(len(encoded)+8)))
	res := exec.Execute(context.Background(), call("two"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want one data part plus the marker", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.DataPart); !ok {
		t.Fatalf("part 0 = %T, want the surviving data part", res.Content.Parts[0])
	}
	if text, ok := res.Content.Parts[1].(message.TextPart); !ok || !strings.Contains(text.Text, "truncated") {
		t.Fatalf("part 1 = %#v, want the truncation marker", res.Content.Parts[1])
	}
}

func TestResultLimiter_PartBudgetLifted(t *testing.T) {
	payload := `{"blob":"` + strings.Repeat("z", 8192) + `"}`
	exec := tool.NewExecutor(catalogWith(dataTool("data", payload)),
		ResultLimiter(1000, WithResultPartBudget(0)))

	res := exec.Execute(context.Background(), call("data"))
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the data part kept", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.DataPart); !ok {
		t.Fatalf("part = %T, want the untouched data part", res.Content.Parts[0])
	}
}

func TestResultLimiter_MarkerKeepsPartOrder(t *testing.T) {
	source, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	mediaTool := tool.FuncTool(message.ToolDefinition{Name: "shot"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: strings.Repeat("x", 500)},
				message.ImagePart{Source: source},
			}}, nil
		})

	// The text budget is exhausted before any text survives, so the
	// marker stands where the cut happened — ahead of the media that
	// followed the dropped text.
	exec := tool.NewExecutor(catalogWith(mediaTool), ResultLimiter(5))
	res := exec.Execute(context.Background(), call("shot"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want the marker plus the image", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.TextPart); !ok {
		t.Fatalf("part 0 = %T, want the marker before the media", res.Content.Parts[0])
	}
	if _, ok := res.Content.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("part 1 = %T, want the untouched image", res.Content.Parts[1])
	}
}
