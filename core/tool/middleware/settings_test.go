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

// TestFromSettings_NonTextPartsBoundedByDefault pins the default that keeps a
// tool result from handing the model an unbounded context item: text stays
// open unless the deployment asks for a rune budget, but media is metered by
// the built-in part budget even when no result_limit is configured.
func TestFromSettings_NonTextPartsBoundedByDefault(t *testing.T) {
	mws, err := FromSettings(Settings{})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	oversized := `{"blob":"` + strings.Repeat("x", DefaultResultPartBudget+1) + `"}`
	exec := tool.NewExecutor(catalogWith(dataTool("data", oversized)), mws...)
	res := exec.Execute(context.Background(), call("data"))
	if len(res.Content.Parts) != 1 {
		t.Fatalf("parts = %d, want the marker alone", len(res.Content.Parts))
	}
	if got := res.Content.Text(); got != DefaultResultMarker {
		t.Fatalf("content = %q, want the default marker", got)
	}

	// Text is still unbounded by default: a large text result passes intact.
	textExec := tool.NewExecutor(catalogWith(longTool(DefaultResultPartBudget+1)), mws...)
	if got := textExec.Execute(context.Background(), call("long")).Content.Text(); len(got) != DefaultResultPartBudget+1 {
		t.Fatalf("text result was shortened to %d bytes without a declared budget", len(got))
	}
}

// TestFromSettings_PartBudgetOverrideAndLift covers the escape hatches: the
// settings-level budget moves the default, and 0 lifts it.
func TestFromSettings_PartBudgetOverrideAndLift(t *testing.T) {
	small := 256
	mws, err := FromSettings(Settings{ResultPartBudgetBytes: &small})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	exec := tool.NewExecutor(catalogWith(
		dataTool("data", `{"blob":"`+strings.Repeat("x", 512)+`"}`)), mws...)
	if parts := exec.Execute(context.Background(), call("data")).Content.Parts; len(parts) != 1 {
		t.Fatalf("parts = %d, want the marker alone under the smaller budget", len(parts))
	}

	unlimited := 0
	mws, err = FromSettings(Settings{ResultPartBudgetBytes: &unlimited})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	exec = tool.NewExecutor(catalogWith(dataTool("data", `{"blob":"`+strings.Repeat("x", 512)+`"}`)), mws...)
	if parts := exec.Execute(context.Background(), call("data")).Content.Parts; len(parts) != 1 {
		t.Fatalf("parts = %d, want the untouched data part", len(parts))
	}
	if _, ok := exec.Execute(context.Background(), call("data")).Content.Parts[0].(message.DataPart); !ok {
		t.Fatal("lifted budget must leave the data part untouched")
	}

	// With a text budget declared, its own part_budget_bytes is the one knob.
	mws, err = FromSettings(Settings{
		ResultLimit:           &ResultLimitSettings{Max: 1000},
		ResultPartBudgetBytes: &small,
	})
	if err != nil {
		t.Fatalf("FromSettings: %v", err)
	}
	exec = tool.NewExecutor(catalogWith(
		dataTool("data", `{"blob":"`+strings.Repeat("x", 512)+`"}`)), mws...)
	parts := exec.Execute(context.Background(), call("data")).Content.Parts
	if len(parts) != 1 {
		t.Fatalf("parts = %d, want the data part alone", len(parts))
	}
	if _, ok := parts[0].(message.DataPart); !ok {
		t.Fatalf(
			"part = %T: result_part_budget_bytes must not override result_limit's own part budget",
			parts[0],
		)
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
