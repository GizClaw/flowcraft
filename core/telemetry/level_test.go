package telemetry_test

import (
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/telemetry"
)

func TestTraceLevelAllows(t *testing.T) {
	t.Parallel()
	cases := []struct {
		level  telemetry.TraceLevel
		wanted telemetry.TraceLevel
		allows bool
	}{
		{telemetry.TraceOff, telemetry.TraceOff, true},
		{telemetry.TraceOff, telemetry.TraceRun, false},
		{telemetry.TraceRun, telemetry.TraceRun, true},
		{telemetry.TraceRun, telemetry.TraceStep, false},
		{telemetry.TraceNode, telemetry.TraceStep, true},
		{telemetry.TraceNode, telemetry.TraceNode, true},
	}
	for _, tc := range cases {
		if got := tc.level.Allows(tc.wanted); got != tc.allows {
			t.Errorf("%s.Allows(%s) = %v, want %v", tc.level, tc.wanted, got, tc.allows)
		}
	}
}

func TestParseTraceLevel(t *testing.T) {
	t.Parallel()
	cases := map[string]telemetry.TraceLevel{
		"":       telemetry.TraceOff,
		"off":    telemetry.TraceOff,
		"run":    telemetry.TraceRun,
		"step":   telemetry.TraceStep,
		"node":   telemetry.TraceNode,
		" Node ": telemetry.TraceNode,
		"NODE":   telemetry.TraceNode,
	}
	for written, wanted := range cases {
		got, err := telemetry.ParseTraceLevel(written)
		if err != nil {
			t.Fatalf("ParseTraceLevel(%q): %v", written, err)
		}
		if got != wanted {
			t.Errorf("ParseTraceLevel(%q) = %s, want %s", written, got, wanted)
		}
	}
}

func TestParseTraceLevelRejectsUnknown(t *testing.T) {
	t.Parallel()
	_, err := telemetry.ParseTraceLevel("everything")
	if err == nil {
		t.Fatal("expected an error for an unknown level")
	}
	// The error must name what the reader may write instead: a typo is the
	// failure mode this check exists for.
	for _, want := range []string{"off", "run", "step", "node"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestTraceLevelStringsRoundTrip(t *testing.T) {
	t.Parallel()
	for _, level := range []telemetry.TraceLevel{
		telemetry.TraceOff, telemetry.TraceRun, telemetry.TraceStep, telemetry.TraceNode,
	} {
		parsed, err := telemetry.ParseTraceLevel(level.String())
		if err != nil {
			t.Fatalf("ParseTraceLevel(%q): %v", level.String(), err)
		}
		if parsed != level {
			t.Errorf("%s round-tripped to %s", level, parsed)
		}
	}
}
