package bytedance

import (
	"context"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/message/media"
	arkresponses "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model/responses"
)

func toolResultRequest(t *testing.T, parts ...message.Part) inference.GenerateRequest {
	t.Helper()
	request := conformanceTextRequest()
	request.Context = append(request.Context, message.Message{
		Role: message.RoleTool,
		Content: message.Content{Parts: []message.Part{
			message.ToolResultPart{Result: message.ToolResult{
				CallID:  "call_1",
				Content: message.Content{Parts: parts},
			}},
		}},
	})
	return request
}

func toolResultImage(t *testing.T) message.Part {
	t.Helper()
	source, err := media.NewImageBytes([]byte("png-bytes"), "image/png")
	if err != nil {
		t.Fatalf("NewImageBytes: %v", err)
	}
	return message.ImagePart{Source: source}
}

// findToolResult returns the compiled function_call_output payload.
func findToolResult(t *testing.T, request *arkresponses.ResponsesRequest) string {
	t.Helper()
	for _, item := range request.Input.GetListValue().ListValue {
		if output := item.GetFunctionToolCallOutput(); output != nil {
			return output.Output
		}
	}
	t.Fatalf("no tool result item in %+v", request.Input)
	return ""
}

// TestToolResultTextRidesVerbatim pins the text path: ark's
// function_call_output is a string, and a text-only result is preserved.
func TestToolResultTextRidesVerbatim(t *testing.T) {
	request := toolResultRequest(t, message.TextPart{Text: "found it"})
	compiled, err := compileGenerate("doubao-seed-2-0-lite", catalog["doubao-seed-2-0-lite"])(
		context.Background(),
		conformanceModel("doubao-seed-2-0-lite"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if output := findToolResult(t, compiled.Wire); output != "found it" {
		t.Fatalf("tool output = %q", output)
	}
	if compiled.Report.Dropped(inference.FieldGenerateContextToolResult) {
		t.Fatalf("text-only result must compile native: %+v", compiled.Report.Decisions)
	}
}

// TestToolResultOmittedPartKeepsPosition pins the limit of the string output:
// a part ark cannot carry becomes an in-place placeholder and a ledger reason,
// never a silent truncation and never a failed turn.
func TestToolResultOmittedPartKeepsPosition(t *testing.T) {
	request := toolResultRequest(t,
		message.TextPart{Text: "before"},
		toolResultImage(t),
		message.TextPart{Text: "after"},
	)
	compiled, err := compileGenerate("doubao-seed-2-0-lite", catalog["doubao-seed-2-0-lite"])(
		context.Background(),
		conformanceModel("doubao-seed-2-0-lite"),
		request,
		inference.GenerateExecutionUnary,
	)
	// A tool result degrades rather than failing the turn: the caller cannot
	// control what a tool returns.
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	field := inference.FieldGenerateContextToolResult
	if !compiled.Report.Dropped(field) {
		t.Fatalf("decisions = %+v", compiled.Report.Decisions)
	}
	reason := ""
	for _, decision := range compiled.Report.Decisions {
		if decision.Field == field {
			reason = decision.Reason
		}
	}
	if !strings.Contains(reason, "ark tool output is text only") {
		t.Fatalf("drop reason = %q", reason)
	}
	output := findToolResult(t, compiled.Wire)
	if !strings.HasPrefix(output, "before") ||
		!strings.Contains(output, "omitted tool output") ||
		!strings.HasSuffix(output, "after") {
		t.Fatalf("tool output = %q", output)
	}
}
