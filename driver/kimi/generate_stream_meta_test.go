package kimi

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

var _ inference.ProviderStreamMetadata = (*chatStream)(nil)

func TestChatStreamMetadataExposesRequestAndResponseIDs(t *testing.T) {
	stream := &chatStream{
		requestID: "req_kimi_1",
		id:        "chatcmpl_kimi_1",
	}
	if got := stream.RequestID(); got != "req_kimi_1" {
		t.Fatalf("RequestID = %q, want req_kimi_1", got)
	}
	if got := stream.ResponseID(); got != "chatcmpl_kimi_1" {
		t.Fatalf("ResponseID = %q, want chatcmpl_kimi_1", got)
	}
}
