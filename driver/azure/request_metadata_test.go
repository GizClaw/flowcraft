package azure

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestRequestMetadataCompilesAndLoweringOptions(t *testing.T) {
	entry := entryFor(ModelSpec{
		Name: "gpt-test",
		Kind: "generate",
		Capabilities: inference.ModelCapabilities{
			Inputs:  []message.PartKind{message.PartText},
			Outputs: []message.PartKind{message.PartText},
		},
	})
	request := inference.GenerateRequest{
		Input: inference.GenerateInput{
			Role: inference.InputRoleUser,
			Content: inference.InputContent{
				Content: message.Content{Parts: []message.Part{
					message.TextPart{Text: "hi"},
				}},
				Intent: inference.Intent{Text: &inference.TextIntent{}},
			},
		},
		RequestMetadata: map[string]string{
			"session_id": "s-1",
			"turn_id":    "t-1",
		},
	}

	typed := entry
	typed.requestMetadataEnvelope = "metadata"
	compiled, err := compileGenerate("gpt-test", typed)(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: "azure", Name: "gpt-test"}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile metadata: %v", err)
	}
	params := wireToParams(compiled.Wire)
	if params.Metadata["session_id"] != "s-1" ||
		params.Metadata["turn_id"] != "t-1" {
		t.Fatalf("metadata = %v, want session + turn", params.Metadata)
	}
	if disposition := metadataDisposition(compiled.Report); disposition != inference.Native {
		t.Fatalf("metadata disposition = %q, want native", disposition)
	}

	client := entry
	client.requestMetadataEnvelope = "client_metadata"
	compiled, err = compileGenerate("gpt-test", client)(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: "azure", Name: "gpt-test"}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile client_metadata: %v", err)
	}
	if options := requestMetadataOptions(compiled.Wire); len(options) != 1 {
		t.Fatalf("client_metadata options = %d, want 1", len(options))
	}
	if disposition := metadataDisposition(compiled.Report); disposition != inference.Native {
		t.Fatalf("metadata disposition = %q, want native", disposition)
	}

	custom := entry
	custom.requestMetadataEnvelope = "request_fields"
	compiled, err = compileGenerate("gpt-test", custom)(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: "azure", Name: "gpt-test"}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile custom envelope: %v", err)
	}
	if options := requestMetadataOptions(compiled.Wire); len(options) != 1 {
		t.Fatalf("custom envelope options = %d, want 1", len(options))
	}

	compiled, err = compileGenerate("gpt-test", entry)(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: "azure", Name: "gpt-test"}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile disabled metadata: %v", err)
	}
	if disposition := metadataDisposition(compiled.Report); disposition != inference.Dropped {
		t.Fatalf("metadata disposition = %q, want dropped", disposition)
	}
}

func metadataDisposition(report inference.CompileReport) inference.Disposition {
	for _, decision := range report.Decisions {
		if decision.Field == inference.FieldGenerateRequestMetadata {
			return decision.Disposition
		}
	}
	return ""
}
