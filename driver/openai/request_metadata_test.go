package openai

import (
	"context"
	"net/http"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestRequestMetadataTypedAndClientEnvelopes(t *testing.T) {
	request := simpleTextRequest("hi")
	request.RequestMetadata = map[string]string{
		"session_id": "s-1",
		"turn_id":    "t-1",
	}

	typed := catalog["gpt-5.6-sol"]
	typed.requestMetadataEnvelope = "metadata"
	compiled, err := compileGenerate("gpt-5.6-sol", typed)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
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

	client := catalog["gpt-5.6-sol"]
	client.requestMetadataEnvelope = "client_metadata"
	compiled, err = compileGenerate("gpt-5.6-sol", client)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
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

	custom := catalog["gpt-5.6-sol"]
	custom.requestMetadataEnvelope = "request_fields"
	compiled, err = compileGenerate("gpt-5.6-sol", custom)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
		inference.GenerateExecutionUnary,
	)
	if err != nil {
		t.Fatalf("compile custom envelope: %v", err)
	}
	if options := requestMetadataOptions(compiled.Wire); len(options) != 1 {
		t.Fatalf("custom envelope options = %d, want 1", len(options))
	}

	disabled := catalog["gpt-5.6-sol"]
	compiled, err = compileGenerate("gpt-5.6-sol", disabled)(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
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

func TestRequestMetadataClientForwardedUnaryBody(t *testing.T) {
	server, capture := newCapturedOpenAI(t, func(
		w http.ResponseWriter,
		_ *http.Request,
		_ map[string]any,
	) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responsesResponseJSON([]map[string]any{
			textOutputItem("ok"),
		})))
	})
	cls := testClients(t, server)
	entry := catalog["gpt-5.6-sol"]
	entry.requestMetadataEnvelope = "client_metadata"
	operations, err := openGenerate(
		cls,
		entry,
		openaiModel("gpt-5.6-sol").ID,
		"default",
	)
	if err != nil {
		t.Fatalf("openGenerate: %v", err)
	}
	request := simpleTextRequest("hi")
	request.RequestMetadata = map[string]string{
		"session_id": "s-1",
		"turn_id":    "t-1",
	}
	if _, err := operations.Unary.Execute(
		context.Background(),
		openaiModel("gpt-5.6-sol"),
		request,
	); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	metadata, ok := capture.body(0)["client_metadata"].(map[string]any)
	if !ok || metadata["session_id"] != "s-1" || metadata["turn_id"] != "t-1" {
		t.Fatalf("client_metadata = %v, want session + turn", capture.body(0)["client_metadata"])
	}
}
