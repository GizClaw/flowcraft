package bytedance

import (
	"context"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestRequestMetadataDroppedByLedger(t *testing.T) {
	request := conformanceTextRequest()
	request.RequestMetadata = map[string]string{"session_id": "s-1"}
	compiled, err := compileGenerate("doubao-seed-2-0-lite", catalog["doubao-seed-2-0-lite"])(
		context.Background(),
		conformanceModel("doubao-seed-2-0-lite"),
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
