package kimi

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/message"
)

func TestRequestMetadataDroppedByLedger(t *testing.T) {
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
		RequestMetadata: map[string]string{"session_id": "s-1"},
	}
	compiled, err := compileGenerate("kimi-k3", catalog["kimi-k3"])(
		context.Background(),
		inference.ModelRef{ID: inference.ModelID{Provider: "kimi", Name: "kimi-k3"}},
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if disposition := requestMetadataDisposition(compiled.Report); disposition != inference.Dropped {
		t.Fatalf("metadata disposition = %q, want dropped", disposition)
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
