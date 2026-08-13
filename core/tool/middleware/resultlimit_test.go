package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func longTool(n int) tool.Tool {
	return tool.FuncTool(message.ToolDefinition{Name: "long"},
		func(_ context.Context, _ string) (string, error) {
			return strings.Repeat("x", n), nil
		})
}

func TestResultLimiter_TruncatesWithMarker(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(1000)), ResultLimiter(100))
	res := exec.Execute(context.Background(), call("long"))
	if res.IsError {
		t.Fatalf("unexpected error result: %q", res.Content)
	}
	if len([]rune(res.Content)) != 100 {
		t.Errorf("limited content length = %d, want 100", len([]rune(res.Content)))
	}
	if !strings.HasSuffix(res.Content, DefaultResultMarker) {
		t.Errorf("content %q does not end with default marker %q", res.Content, DefaultResultMarker)
	}
}

func TestResultLimiter_UnderLimitUntouched(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(50)), ResultLimiter(100))
	res := exec.Execute(context.Background(), call("long"))
	if res.Content != strings.Repeat("x", 50) {
		t.Errorf("content = %q, want the full 50 runes", res.Content)
	}
}

func TestResultLimiter_TruncatesErrorResults(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(tool.FuncTool(
		message.ToolDefinition{Name: "boom"},
		func(_ context.Context, _ string) (string, error) {
			return "", &errLike{}
		})), ResultLimiter(20))
	res := exec.Execute(context.Background(), call("boom"))
	if !res.IsError {
		t.Fatal("expected IsError to survive truncation")
	}
	if len([]rune(res.Content)) != 20 {
		t.Errorf("limited error length = %d, want 20", len([]rune(res.Content)))
	}
}

type errLike struct{}

func (e *errLike) Error() string { return strings.Repeat("y", 500) }

func TestResultLimiter_CustomMarker(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(1000)),
		ResultLimiter(64, WithResultMarker("[cut here]")))
	res := exec.Execute(context.Background(), call("long"))
	if !strings.HasSuffix(res.Content, "[cut here]") {
		t.Errorf("content %q does not end with custom marker", res.Content)
	}
}

func TestResultLimiter_TrimsMarkerToFitTinyLimit(t *testing.T) {
	exec := tool.NewExecutor(catalogWith(longTool(1000)),
		ResultLimiter(3, WithResultMarker("MARK")))
	res := exec.Execute(context.Background(), call("long"))
	if res.Content != "MAR" {
		t.Errorf("content = %q, want marker trimmed to 3 runes", res.Content)
	}
}

func TestResultLimiter_DoesNotSplitRunes(t *testing.T) {
	content := strings.Repeat("你", 100)
	exec := tool.NewExecutor(catalogWith(tool.FuncTool(
		message.ToolDefinition{Name: "cjk"},
		func(_ context.Context, _ string) (string, error) { return content, nil })),
		ResultLimiter(40))
	res := exec.Execute(context.Background(), call("cjk"))
	if !strings.HasSuffix(res.Content, DefaultResultMarker) {
		t.Fatalf("content %q does not end with marker", res.Content)
	}
	if len([]rune(res.Content)) != 40 {
		t.Errorf("limited content length = %d, want 40 runes", len([]rune(res.Content)))
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
