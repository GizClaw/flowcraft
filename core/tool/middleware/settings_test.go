package middleware

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

func TestFromSettings_ResultLimitTruncatesText(t *testing.T) {
	mws, err := FromSettings(Settings{ResultLimit: &ResultLimitSettings{Max: 40}})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}

	exec := tool.NewExecutor(catalogWith(longTool(500)), mws...)
	res := exec.Execute(context.Background(), call("long"))
	if got := len([]rune(res.Content.Text())); got != 40 {
		t.Fatalf("limited text = %d runes, want 40", got)
	}
	if !strings.HasSuffix(res.Content.Text(), DefaultResultMarker) {
		t.Fatalf("content = %q, want the default marker", res.Content.Text())
	}
}

func TestFromSettings_ResultLimitMarkerAndPartBudget(t *testing.T) {
	budget := 128
	mws, err := FromSettings(Settings{ResultLimit: &ResultLimitSettings{
		Max:             1000,
		Marker:          "[cut]",
		PartBudgetBytes: &budget,
	}})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}

	exec := tool.NewExecutor(catalogWith(dataTool("data", `{"blob":"`+strings.Repeat("x", 2048)+`"}`)), mws...)
	res := exec.Execute(context.Background(), call("data"))
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the marker alone", len(res.Content.Parts))
	}
	if got := res.Content.Text(); got != "[cut]" {
		t.Fatalf("content = %q, want the settings marker", got)
	}
}

func TestFromSettings_ResultLimitLiftsPartBudget(t *testing.T) {
	unlimited := 0
	mws, err := FromSettings(Settings{ResultLimit: &ResultLimitSettings{
		Max:             1000,
		PartBudgetBytes: &unlimited,
	}})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}

	exec := tool.NewExecutor(catalogWith(dataTool("data", `{"blob":"`+strings.Repeat("x", 8192)+`"}`)), mws...)
	res := exec.Execute(context.Background(), call("data"))
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the data part kept", len(res.Content.Parts))
	}
	if _, ok := res.Content.Parts[0].(message.DataPart); !ok {
		t.Fatalf("part = %T, want the untouched data part", res.Content.Parts[0])
	}
}

func TestFromSettings_ResultLimitRequiresPositiveMax(t *testing.T) {
	for _, max := range []int{0, -1} {
		_, err := FromSettings(Settings{ResultLimit: &ResultLimitSettings{Max: max}})
		if !errdefs.IsValidation(err) {
			t.Fatalf("max = %d: err = %v, want Validation", max, err)
		}
	}
}

// The limiter sits outside the timeout, so the timeout's own error
// result is bounded too.
func TestFromSettings_ResultLimitWrapsTimeout(t *testing.T) {
	mws, err := FromSettings(Settings{
		ResultLimit: &ResultLimitSettings{Max: 30, Marker: "…"},
		Timeout:     &TimeoutSettings{Default: "10ms"},
	})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}

	hang := tool.TextTool(message.ToolDefinition{Name: "hang"},
		func(ctx context.Context, _ string) (string, error) {
			<-ctx.Done()
			return "", ctx.Err()
		})
	exec := tool.NewExecutor(catalogWith(hang), mws...)
	res := exec.Execute(context.Background(), call("hang"))
	if !res.IsError {
		t.Fatalf("result = %+v, want the timeout error result", res)
	}
	if got := res.Content.Text(); !strings.Contains(got, "timed out") || !strings.HasSuffix(got, "…") {
		t.Fatalf("content = %q, want the limited timeout message", got)
	}
}
