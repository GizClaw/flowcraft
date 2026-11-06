package engine_test

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/sandbox"
	"github.com/GizClaw/flowcraft/core/sandbox/engine"
	"github.com/GizClaw/flowcraft/core/telemetry"
	"github.com/GizClaw/flowcraft/core/telemetry/telemetrytest"

	"go.opentelemetry.io/otel/attribute"
)

// TestExecEmitsSpan pins that one executed command is one span carrying the exit
// code: a non-zero exit is a result the tool layer interprets, not a telemetry
// failure, so the status stays Ok and the code tells the story.
func TestExecEmitsSpan(t *testing.T) {
	skipOnWindows(t)
	recorder := telemetrytest.New(t)

	result, err := engine.Exec(
		context.Background(), localRunner(t), "echo", []string{"hi"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}

	span := recorder.RequireNamed(t, "sandbox.exec")
	telemetrytest.RequireAttr(t, span, telemetry.AttrSandboxExitCode, attribute.IntValue(0))
	telemetrytest.RequireAttrString(t, span, telemetry.AttrSandboxRunner, "*local.Runner")
}

// TestExecSpanRecordsNonZeroExit pins that a failing command is still a
// successful span: the agent asked for the command, it ran, and it exited 3.
func TestExecSpanRecordsNonZeroExit(t *testing.T) {
	skipOnWindows(t)
	recorder := telemetrytest.New(t)

	result, err := engine.Exec(
		context.Background(), localRunner(t), "sh", []string{"-c", "exit 3"}, sandbox.ExecOptions{})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if result.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", result.ExitCode)
	}

	span := recorder.RequireNamed(t, "sandbox.exec")
	telemetrytest.RequireAttr(t, span, telemetry.AttrSandboxExitCode, attribute.IntValue(3))
}
