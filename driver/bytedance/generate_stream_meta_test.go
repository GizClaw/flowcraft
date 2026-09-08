package bytedance

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"

	"github.com/volcengine/volcengine-go-sdk/service/arkruntime"
	arkmodel "github.com/volcengine/volcengine-go-sdk/service/arkruntime/model"
)

var _ inference.ProviderStreamMetadata = (*responsesStream)(nil)

func TestResponsesStreamMetadataCapturesRequestID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/responses" {
			t.Errorf("path = %q, want /responses", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set(arkmodel.ClientRequestHeader, "req-ark-1")
		_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	defer server.Close()

	client := arkruntime.NewClientWithApiKey(
		"test-key",
		arkruntime.WithBaseUrl(server.URL),
	)
	compiled, err := compileGenerate(
		"doubao-seed-2-0-lite",
		catalog["doubao-seed-2-0-lite"],
	)(
		context.Background(),
		conformanceModel("doubao-seed-2-0-lite"),
		conformanceTextRequest(),
		inference.GenerateExecutionStream,
	)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	stream, err := transportGenerateStream(client)(context.Background(), compiled.Wire)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer func() { _ = stream.Close() }()

	meta, ok := stream.(inference.ProviderStreamMetadata)
	if !ok {
		t.Fatal("responses stream must expose provider stream metadata")
	}
	if got := meta.RequestID(); got != "req-ark-1" {
		t.Fatalf("stream request id = %q, want req-ark-1", got)
	}
}
