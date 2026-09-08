package azure

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
	"github.com/GizClaw/flowcraft/core/resource"
)

var _ inference.ProviderStreamMetadata = (*responsesStream)(nil)
var _ inference.ProviderStreamMetadata = (*imageStream)(nil)

func TestResponsesStreamMetadataSurfacesRequestID(t *testing.T) {
	stream := &responsesStream{requestID: "req_azure_1"}
	if got := stream.RequestID(); got != "req_azure_1" {
		t.Fatalf("RequestID = %q, want req_azure_1", got)
	}
}

func TestImageStreamMetadataSurfacesRequestID(t *testing.T) {
	stream := &imageStream{requestID: "req_azure_img_1"}
	if got := stream.RequestID(); got != "req_azure_img_1" {
		t.Fatalf("RequestID = %q, want req_azure_img_1", got)
	}
}

func TestResponsesStreamTransportCapturesRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/responses" {
			t.Errorf("path = %q, want /openai/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("x-request-id", "req_azure_responses_1")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	spec, err := decodeSpec(context.Background(), []byte(fmt.Sprintf(`{
		"endpoint": %q,
		"models": [{
			"name": "deploy",
			"kind": "generate",
			"capabilities": {"outputs": ["text"]}
		}]
	}`, server.URL)))
	if err != nil {
		t.Fatalf("decodeSpec: %v", err)
	}
	cls, err := profileMaterial{
		apiKey: resource.LiteralSecret("test-key"),
	}.newClients(context.Background(), spec)
	if err != nil {
		t.Fatalf("newClients: %v", err)
	}

	compiled, err := compileGenerate("deploy", entryFor(ModelSpec{Name: "deploy", Kind: "generate"}))(
		context.Background(),
		conformanceModel("deploy"),
		conformanceTextRequest(),
		inference.GenerateExecutionStream,
	)
	if err != nil {
		t.Fatalf("compile responses stream: %v", err)
	}
	stream, err := transportGenerateStream(cls.api)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	meta, ok := stream.(inference.ProviderStreamMetadata)
	if !ok {
		t.Fatal("responses stream must expose provider stream metadata")
	}
	if got := meta.RequestID(); got != "req_azure_responses_1" {
		t.Fatalf("stream request id = %q, want req_azure_responses_1", got)
	}
}
