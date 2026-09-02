package deepseek

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestRequestMetadataLedgerDisposition(t *testing.T) {
	request := metadataTextRequest("hi")

	disabled := chatEntry()
	compiled, err := compileChatGenerate("deepseek-v4-flash", disabled)(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile disabled: %v", err)
	}
	if disposition := requestMetadataDisposition(compiled.Report); disposition != inference.Dropped {
		t.Fatalf("chat disposition = %q, want dropped", disposition)
	}

	enabled := chatEntryWithMetadata("custom_fields")
	compiled, err = compileChatGenerate("deepseek-v4-flash", enabled)(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile enabled: %v", err)
	}
	if disposition := requestMetadataDisposition(compiled.Report); disposition != inference.Native {
		t.Fatalf("chat disposition = %q, want native", disposition)
	}
	if _, overrides := wireToChatParams(compiled.Wire); len(overrides) != 1 {
		t.Fatalf("chat overrides = %d, want 1", len(overrides))
	}

	enabledResponses := responsesEntryWithMetadata("custom_fields")
	responsesCompiled, err := compileResponsesGenerate("deepseek-v4-flash", enabledResponses)(
		context.Background(),
		deepseekModel("deepseek-v4-flash"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile responses enabled: %v", err)
	}
	if disposition := requestMetadataDisposition(responsesCompiled.Report); disposition != inference.Native {
		t.Fatalf("responses disposition = %q, want native", disposition)
	}
	if options := responseMetadataOptions(responsesCompiled.Wire); len(options) != 1 {
		t.Fatalf("responses options = %d, want 1", len(options))
	}
}

func requestMetadataDisposition(report inference.CompileReport) inference.Disposition {
	for _, decision := range report.Decisions {
		if decision.Field == inference.FieldGenerateRequestMetadata {
			return decision.Disposition
		}
	}
	return ""
}
