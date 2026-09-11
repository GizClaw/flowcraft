package anthropic

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

// TestToolResultVideoBecomesPlaceholder pins the tool_result content union's
// limit: it carries text and images only, so a video part is replaced in place
// and reported on the ledger even when the endpoint accepts video blocks
// elsewhere. Before this contract the part lowered to an empty text block
// while the ledger counted it as carried.
func TestToolResultVideoBecomesPlaceholder(t *testing.T) {
	entry := declaredVideoEntry(t, true)
	request := toolResultRequest(t,
		message.TextPart{Text: "before"},
		toolResultVideo(t),
		message.TextPart{Text: "after"},
	)
	compiled, err := compileGenerate("m", entry)(
		context.Background(),
		conformanceModel("m"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}

	field := inference.FieldGenerateContextToolResult
	if !compiled.Report.Dropped(field) {
		t.Fatalf("video in a tool result must drop with a reason: %+v", compiled.Report.Decisions)
	}
	reason := ""
	for _, decision := range compiled.Report.Decisions {
		if decision.Field == field {
			reason = decision.Reason
		}
	}
	if !strings.Contains(reason, "tool results carry text and images only") {
		t.Fatalf("drop reason = %q", reason)
	}

	block := findToolResult(t, compiled.Wire)
	if len(block.Content) != 3 ||
		block.Content[1].OfText == nil ||
		!strings.Contains(block.Content[1].OfText.Text, "omitted tool output") {
		t.Fatalf("result blocks = %+v", block.Content)
	}
}
