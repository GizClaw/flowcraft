package middleware

import (
	"context"
	"slices"
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

// TestFromSettings_HostileValues walks every Settings field with the values a
// deployment document can reach and pins two promises at once: a value that
// cannot be honoured is reported as an errdefs.Validation error, and no
// constructor in the chain panics while rejecting it. Zero and negative part
// budgets, an empty timeout and a disabled truncate stay valid: they are the
// documented escape hatches, not misconfiguration.
func TestFromSettings_HostileValues(t *testing.T) {
	zero, negative := 0, -1
	overMax := int(^uint(0) >> 1)
	dir := t.TempDir()

	tests := []struct {
		name     string
		settings Settings
		wantErr  bool
	}{
		{"empty settings", Settings{}, false},
		{"recover disabled", Settings{Recover: &RecoverSettings{Enabled: boolPtr(false)}}, false},
		{"recover enabled", Settings{Recover: &RecoverSettings{Enabled: boolPtr(true)}}, false},
		{"telemetry disabled", Settings{Telemetry: &TelemetrySettings{Enabled: false}}, false},
		{"telemetry enabled", Settings{Telemetry: &TelemetrySettings{Enabled: true}}, false},
		{
			"timeout empty default",
			Settings{Timeout: &TimeoutSettings{Default: ""}},
			false,
		},
		{
			"timeout malformed duration",
			Settings{Timeout: &TimeoutSettings{Default: "soon"}},
			true,
		},
		{
			"timeout overflowing duration",
			Settings{Timeout: &TimeoutSettings{Default: "1000000000000h"}},
			true,
		},
		{
			"timeout negative duration",
			Settings{Timeout: &TimeoutSettings{Default: "-5s"}},
			false,
		},
		{
			"concurrency zero limit",
			Settings{Concurrency: &ConcurrencySettings{Limit: 0}},
			true,
		},
		{
			"concurrency negative limit",
			Settings{Concurrency: &ConcurrencySettings{Limit: -3}},
			true,
		},
		{
			"result_limit zero max",
			Settings{ResultLimit: &ResultLimitSettings{Max: 0}},
			true,
		},
		{
			"result_limit negative max",
			Settings{ResultLimit: &ResultLimitSettings{Max: -1}},
			true,
		},
		{
			"result_limit over-max max",
			Settings{ResultLimit: &ResultLimitSettings{Max: overMax}},
			false,
		},
		{
			"result_limit empty marker",
			Settings{ResultLimit: &ResultLimitSettings{Max: 10, Marker: ""}},
			false,
		},
		{
			"result_limit zero part budget",
			Settings{ResultLimit: &ResultLimitSettings{Max: 10, PartBudgetBytes: &zero}},
			false,
		},
		{
			"result_limit negative part budget",
			Settings{ResultLimit: &ResultLimitSettings{Max: 10, PartBudgetBytes: &negative}},
			false,
		},
		{
			"result_limit over-max part budget",
			Settings{ResultLimit: &ResultLimitSettings{Max: 10, PartBudgetBytes: &overMax}},
			false,
		},
		{
			"truncate disabled with hostile values",
			Settings{Truncate: &TruncateSettings{Enabled: false, MaxChars: 0, Dir: ""}},
			false,
		},
		{
			"truncate zero max chars",
			Settings{Truncate: &TruncateSettings{Enabled: true, MaxChars: 0, Dir: dir}},
			true,
		},
		{
			"truncate negative max chars",
			Settings{Truncate: &TruncateSettings{Enabled: true, MaxChars: -1, Dir: dir}},
			true,
		},
		{
			"truncate empty dir",
			Settings{Truncate: &TruncateSettings{Enabled: true, MaxChars: 10, Dir: ""}},
			true,
		},
		{
			"truncate blank dir",
			Settings{Truncate: &TruncateSettings{Enabled: true, MaxChars: 10, Dir: "   "}},
			true,
		},
		{
			"truncate over-max max chars",
			Settings{Truncate: &TruncateSettings{Enabled: true, MaxChars: overMax, Dir: dir}},
			false,
		},
		{
			"result_part_budget_bytes zero",
			Settings{ResultPartBudgetBytes: &zero},
			false,
		},
		{
			"result_part_budget_bytes negative",
			Settings{ResultPartBudgetBytes: &negative},
			false,
		},
		{
			"result_part_budget_bytes over-max",
			Settings{ResultPartBudgetBytes: &overMax},
			false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mws, recovered, err := fromSettingsRecovering(test.settings)
			if recovered != nil {
				t.Fatalf("FromSettings panicked: %v", recovered)
			}
			if test.wantErr {
				if !errdefs.IsValidation(err) {
					t.Fatalf("err = %v, want a Validation error", err)
				}
				if len(mws) != 0 {
					t.Fatalf("middlewares = %d, want none on a rejected setting", len(mws))
				}
				return
			}
			if err != nil {
				t.Fatalf("FromSettings: %v", err)
			}
		})
	}
}

// TestFromSettings_RecoverDefaultsOn pins the one entry in the chain a
// document has to opt out of. Recovery is the difference between a broken
// third-party tool and a dead host process, so the zero value — an absent
// block, an empty block, an absent enabled field — installs it, and only an
// explicit false goes without.
func TestFromSettings_RecoverDefaultsOn(t *testing.T) {
	tests := []struct {
		name        string
		settings    Settings
		wantHandled bool
	}{
		{"absent block", Settings{}, true},
		{"empty block", Settings{Recover: &RecoverSettings{}}, true},
		{"explicit true", Settings{Recover: &RecoverSettings{Enabled: boolPtr(true)}}, true},
		{"explicit false", Settings{Recover: &RecoverSettings{Enabled: boolPtr(false)}}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mws, err := FromSettings(test.settings)
			if err != nil {
				t.Fatalf("FromSettings: %v", err)
			}
			// The chain is dispatched directly: Executor.Execute has a
			// recovery net of its own, which would hide whether the
			// settings installed the middleware.
			escaped := panicThrough(mws)
			if got := escaped == nil; got != test.wantHandled {
				t.Fatalf("panic escaped = %v (%v), want handled = %v",
					escaped, escaped, test.wantHandled)
			}
		})
	}
}

// panicThrough dispatches one call through mws and reports the panic that
// escaped, if any.
func panicThrough(mws []tool.Middleware) (escaped any) {
	dispatch := tool.Dispatch(func(context.Context, message.ToolCall) message.ToolResult {
		panic("tool is broken")
	})
	for _, mw := range slices.Backward(mws) {
		dispatch = mw(dispatch)
	}
	defer func() { escaped = recover() }()
	dispatch(context.Background(), message.ToolCall{ID: "call", Name: "boom"})
	return escaped
}

func boolPtr(value bool) *bool { return &value }

// fromSettingsRecovering calls FromSettings and returns an escaping panic as a
// value, so a constructor that panics on a hostile setting fails its table row
// instead of aborting the whole run.
func fromSettingsRecovering(s Settings) (mws []tool.Middleware, recovered any, err error) {
	defer func() {
		if r := recover(); r != nil {
			recovered = r
		}
	}()
	mws, err = FromSettings(s)
	return mws, nil, err
}
