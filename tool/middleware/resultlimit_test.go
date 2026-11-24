package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/GizClaw/flowcraft/core/tool/tooltest"
	"github.com/GizClaw/flowcraft/tool/dispatch"
)

func longTool(n int) tool.Tool {
	return tool.TextTool(message.ToolDefinition{Name: "long"},
		func(_ context.Context, _ string) (string, error) {
			return strings.Repeat("x", n), nil
		})
}

func TestResultLimiter_TruncatesWithMarker(t *testing.T) {
	exec := dispatch.New(catalogWith(longTool(1000)), MustResultLimiter(100))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("long"))
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
	exec := dispatch.New(catalogWith(longTool(50)), MustResultLimiter(100))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("long"))
	if res.Content.Text() != strings.Repeat("x", 50) {
		t.Errorf("content = %q, want the full 50 runes", res.Content.Text())
	}
}

func TestResultLimiter_TruncatesErrorResults(t *testing.T) {
	exec := dispatch.New(catalogWith(tool.TextTool(
		message.ToolDefinition{Name: "boom"},
		func(_ context.Context, _ string) (string, error) {
			return "", &errLike{}
		})), MustResultLimiter(20))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("boom"))
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
	exec := dispatch.New(catalogWith(longTool(1000)),
		MustResultLimiter(64, WithResultMarker("[cut here]")))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("long"))
	if !strings.HasSuffix(res.Content.Text(), "[cut here]") {
		t.Errorf("content %q does not end with custom marker", res.Content.Text())
	}
}

func TestResultLimiter_TrimsMarkerToFitTinyLimit(t *testing.T) {
	exec := dispatch.New(catalogWith(longTool(1000)),
		MustResultLimiter(3, WithResultMarker("MARK")))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("long"))
	if res.Content.Text() != "MAR" {
		t.Errorf("content = %q, want marker trimmed to 3 runes", res.Content.Text())
	}
}

func TestResultLimiter_DoesNotSplitRunes(t *testing.T) {
	content := strings.Repeat("你", 100)
	exec := dispatch.New(catalogWith(tool.TextTool(
		message.ToolDefinition{Name: "cjk"},
		func(_ context.Context, _ string) (string, error) { return content, nil })),
		MustResultLimiter(40))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("cjk"))
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

	exec := dispatch.New(catalogWith(mediaTool), MustResultLimiter(50))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("shot"))
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

	exec := dispatch.New(catalogWith(mediaTool), MustResultLimiter(5))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("shot"))
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
	exec := dispatch.New(catalogWith(mediaTool), MustResultLimiter(30))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("shot"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want 2 (nothing to truncate)", len(res.Content.Parts))
	}
	if text, ok := res.Content.Parts[0].(message.TextPart); !ok || text.Text != "caption" {
		t.Fatalf("part 0 = %#v, want the untouched caption", res.Content.Parts[0])
	}
}

func TestResultLimiter_InvalidMaxIsValidationError(t *testing.T) {
	for _, max := range []int{0, -1} {
		limiter, err := ResultLimiter(max)
		if !errdefs.IsValidation(err) {
			t.Fatalf("max = %d: err = %v, want Validation", max, err)
		}
		if limiter != nil {
			t.Fatalf("max = %d: middleware = %v, want nil", max, limiter)
		}
	}
}

func TestMustResultLimiter_InvalidMaxPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for non-positive max")
		}
	}()
	MustResultLimiter(0)
}

// TestResultLimiter_PartsCutKeepsTextBudget pins the shared budget promise: a
// result whose non-text parts were dropped still carries at most max runes of
// text, because the marker's runes are reserved rather than added on top.
func TestResultLimiter_PartsCutKeepsTextBudget(t *testing.T) {
	// A media part on purpose: a structured part that does not fit becomes a note
	// (its own branch), while media is dropped and needs the marker.
	src, err := media.NewImageBytes(make([]byte, 4096), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	caption := strings.Repeat("c", 30)
	exec := dispatch.New(catalogWith(tool.FuncTool(
		message.ToolDefinition{Name: "cap"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: caption},
				message.ImagePart{Source: src},
			}}, nil
		},
	)), MustResultLimiter(30, WithResultPartBudget(256)))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("cap"))
	text := res.Content.Text()
	if got := len([]rune(text)); got > 30 {
		t.Fatalf("text = %d runes, want at most the 30-rune budget (%q)", got, text)
	}
	if !strings.HasSuffix(text, DefaultResultMarker) {
		t.Fatalf("content = %q, want the truncation marker", text)
	}
	for _, part := range res.Content.Parts {
		if _, ok := part.(message.ImagePart); ok {
			t.Fatal("the oversized media part survived the byte budget")
		}
	}
}

// TestResultLimiter_BothCutsKeepOneMarker pins the case where text and parts
// are both over budget: the result stays within max runes and carries one
// marker, not one marker per budget.
func TestResultLimiter_BothCutsKeepOneMarker(t *testing.T) {
	src, err := media.NewImageBytes(make([]byte, 4096), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	exec := dispatch.New(catalogWith(tool.FuncTool(
		message.ToolDefinition{Name: "both"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: strings.Repeat("c", 100)},
				message.ImagePart{Source: src},
			}}, nil
		},
	)), MustResultLimiter(20, WithResultPartBudget(256)))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("both"))
	text := res.Content.Text()
	if got := len([]rune(text)); got != 20 {
		t.Fatalf("text = %d runes (%q), want the full 20-rune budget", got, text)
	}
	if got := strings.Count(text, DefaultResultMarker); got != 1 {
		t.Fatalf("markers = %d in %q, want exactly one", got, text)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// TestResultPartLimiter_BoundsMediaOnly pins the media-only limiter: text
// passes through untouched, oversized non-text parts are dropped with the
// marker, and a result within budget is returned as is.
func TestResultPartLimiter_BoundsNonTextOnly(t *testing.T) {
	mediaTool := tool.FuncTool(message.ToolDefinition{Name: "shot"},
		func(_ context.Context, _ string) (message.Content, error) {
			data, err := message.NewJSONContent(
				[]byte(`{"blob":"` + strings.Repeat("x", 512) + `"}`),
			)
			if err != nil {
				return message.Content{}, err
			}
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: strings.Repeat("c", 4096)},
				data.Parts[0],
			}}, nil
		})

	exec := dispatch.New(catalogWith(mediaTool), MustResultPartLimiter(256))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("shot"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want the text part plus the marker", len(res.Content.Parts))
	}
	text, ok := res.Content.Parts[0].(message.TextPart)
	if !ok {
		t.Fatalf("part 0 = %T, want the untouched text part", res.Content.Parts[0])
	}
	if len([]rune(text.Text)) != 4096 {
		t.Fatalf("text was shortened to %d runes, want it untouched", len([]rune(text.Text)))
	}
	if note, ok := res.Content.Parts[1].(message.TextPart); !ok ||
		!strings.Contains(note.Text, "structured result omitted") {
		t.Fatalf("part 1 = %#v, want the note naming the omitted structured result", res.Content.Parts[1])
	}

	within := dispatch.New(catalogWith(dataTool("data", `{"answer":42}`)), MustResultPartLimiter(256))
	parts := within.Execute(context.Background(), tooltest.Binding(), call("data")).Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the untouched data part", len(parts))
	}
	if _, ok := parts[0].(message.DataPart); !ok {
		t.Fatalf("part = %T, want message.DataPart", parts[0])
	}
}

func TestResultPartLimiter_InvalidBudgetIsValidationError(t *testing.T) {
	for _, budget := range []int{0, -1} {
		limiter, err := ResultPartLimiter(budget)
		if !errdefs.IsValidation(err) {
			t.Fatalf("budget = %d: err = %v, want Validation", budget, err)
		}
		if limiter != nil {
			t.Fatalf("budget = %d: middleware = %v, want nil", budget, limiter)
		}
	}
}

func TestMustResultPartLimiter_InvalidBudgetPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("expected panic for non-positive budget")
		}
	}()
	MustResultPartLimiter(0)
}

// dataTool returns a tool whose result is one structured data part
// carrying payload.
func dataTool(name, payload string) tool.Tool {
	return tool.FuncTool(message.ToolDefinition{Name: name},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.NewJSONContent([]byte(payload))
		})
}

func TestResultLimiter_ReplacesOversizedStructuredPart(t *testing.T) {
	payload := `{"blob":"` + strings.Repeat("x", 2048) + `"}`
	exec := dispatch.New(catalogWith(dataTool("data", payload)),
		MustResultLimiter(1000, WithResultPartBudget(256)))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("data"))
	if res.IsError {
		t.Fatalf("unexpected error result: %s", res.Content.Text())
	}
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the note alone", len(res.Content.Parts))
	}
	// Not silent: the note says a structured result existed and how big it was,
	// where dropping the part said only "something was cut".
	got := res.Content.Text()
	if !strings.Contains(got, "structured result omitted") || !strings.Contains(got, "KiB") {
		t.Fatalf("content = %q, want a note naming the omitted result and its size", got)
	}
	if strings.Contains(got, DefaultResultMarker) {
		t.Fatalf("content = %q, want no second marker: the note is the explanation", got)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestResultLimiter_KeepsPartWithinByteBudget(t *testing.T) {
	payload := `{"answer":42}`
	exec := dispatch.New(catalogWith(dataTool("data", payload)),
		MustResultLimiter(1000, WithResultPartBudget(4096)))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("data"))
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

	// The budget covers exactly one part, so the second is replaced by a note.
	exec := dispatch.New(catalogWith(two),
		MustResultLimiter(1000, WithResultPartBudget(len(encoded)+8)))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("two"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want one data part plus the note", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.DataPart); !ok {
		t.Fatalf("part 0 = %T, want the surviving data part", res.Content.Parts[0])
	}
	if text, ok := res.Content.Parts[1].(message.TextPart); !ok ||
		!strings.Contains(text.Text, "structured result omitted") {
		t.Fatalf("part 1 = %#v, want the note naming the second part", res.Content.Parts[1])
	}
}

// TestResultLimiter_StructuredBudgetFollowsTheTextBudget: a host that allows a
// small text budget is not asking for a megabyte of JSON, so the structured budget
// scales with it (four bytes per rune) instead of staying at the media default.
func TestResultLimiter_StructuredBudgetFollowsTheTextBudget(t *testing.T) {
	big := `{"blob":"` + strings.Repeat("b", 4096) + `"}`
	exec := dispatch.New(catalogWith(dataTool("data", big)), MustResultLimiter(512))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("data"))
	if got := res.Content.Text(); !strings.Contains(got, "structured result omitted") {
		t.Fatalf("content = %q, want the derived budget to refuse the result", got)
	}

	small := `{"blob":"` + strings.Repeat("s", 512) + `"}`
	within := dispatch.New(catalogWith(dataTool("small", small)), MustResultLimiter(512))
	parts := within.Execute(context.Background(), tooltest.Binding(), call("small")).Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the data part kept under the derived budget", len(parts))
	}
}

// TestResultLimiter_StructuredBudgetLifted: the escape hatch for a host that
// bounds JSON elsewhere.
func TestResultLimiter_StructuredBudgetLifted(t *testing.T) {
	big := `{"blob":"` + strings.Repeat("b", 4096) + `"}`
	exec := dispatch.New(catalogWith(dataTool("data", big)),
		MustResultLimiter(512, WithResultStructuredBudget(0)))

	parts := exec.Execute(context.Background(), tooltest.Binding(), call("data")).Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the data part kept", len(parts))
	}
	if _, ok := parts[0].(message.DataPart); !ok {
		t.Fatalf("part = %T, want the untouched data part", parts[0])
	}
}

func TestResultLimiter_PartBudgetLifted(t *testing.T) {
	payload := `{"blob":"` + strings.Repeat("z", 8192) + `"}`
	exec := dispatch.New(catalogWith(dataTool("data", payload)),
		MustResultLimiter(1000, WithResultPartBudget(0)))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("data"))
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the data part kept", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.DataPart); !ok {
		t.Fatalf("part = %T, want the untouched data part", res.Content.Parts[0])
	}
}

// TestResultLimiter_LiftedMediaCapKeepsMedia pins the two caps as independent:
// WithResultPartBudget(0) lifts the media budget, and a structured budget set
// beside it must not turn that into "drop every media part".
func TestResultLimiter_LiftedMediaCapKeepsMedia(t *testing.T) {
	src, err := media.NewImageBytes([]byte{1, 2, 3}, "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	exec := dispatch.New(catalogWith(tool.FuncTool(
		message.ToolDefinition{Name: "shot"},
		func(_ context.Context, _ string) (message.Content, error) {
			return message.Content{Parts: []message.Part{
				message.TextPart{Text: strings.Repeat("c", 100)},
				message.ImagePart{Source: src},
			}}, nil
		},
	)), MustResultLimiter(1000, WithResultPartBudget(0), WithResultStructuredBudget(4096)))

	res := exec.Execute(context.Background(), tooltest.Binding(), call("shot"))
	if len(res.Content.Parts) != 2 {
		t.Fatalf("parts = %d, want the text and the image: a lifted media cap drops nothing", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[1].(message.ImagePart); !ok {
		t.Fatalf("part 1 = %T, want the untouched image", res.Content.Parts[1])
	}
	if got := res.Content.Text(); strings.Contains(got, DefaultResultMarker) {
		t.Fatalf("content = %q, want no marker: nothing was cut", got)
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
	exec := dispatch.New(catalogWith(mediaTool), MustResultLimiter(5))
	res := exec.Execute(context.Background(), tooltest.Binding(), call("shot"))
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
