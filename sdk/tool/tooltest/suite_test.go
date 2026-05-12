package tooltest_test

// Self-tests for the tooltest contract suite.
//
// We deliberately keep this file thin: only the positive case
// (a known-compliant tool passes RunSuite) is exercised here.
// Negative cases (RunSuite must FLAG a non-compliant tool) cannot
// be expressed in idiomatic Go *testing.T plumbing — *testing.T
// is a struct, not an interface, so swapping in a "recording" T
// to capture would-be failures is not possible without forking
// the testing package. Instead, the negative path is covered by
// a) careful code review of suite.go (each subtest is small and
// reads top-down) and b) every real consumer of RunSuite (askuser
// and future built-in tools) actually exercising the suite — if
// the suite went silently lenient, those consumer tests would
// stop catching their target regressions in code review.

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/model"
	"github.com/GizClaw/flowcraft/sdk/tool"
	"github.com/GizClaw/flowcraft/sdk/tool/tooltest"
)

// goodTool is a minimal compliant Tool: non-empty Name, JSON-object
// schema, accepts empty args, returns Validation on bad JSON, and
// honours ctx cancel. It exists so we can self-test that RunSuite
// passes a known-good implementation — if THIS test ever flunks,
// the suite has acquired a false-positive that would falsely fail
// real tools too.
type goodTool struct{}

func (goodTool) Definition() model.ToolDefinition {
	return model.ToolDefinition{
		Name:        "good",
		Description: "the contract-compliant baseline",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (g goodTool) Execute(ctx context.Context, args string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if args == "{" {
		return "", errdefs.Validationf("bad json")
	}
	return "ok", nil
}

func TestRunSuite_PassesWellBehavedTool(t *testing.T) {
	tooltest.RunSuite(t, func() tool.Tool { return goodTool{} })
}
