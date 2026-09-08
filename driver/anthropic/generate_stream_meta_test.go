package anthropic

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

var _ inference.ProviderStreamMetadata = (*messagesStream)(nil)

func TestMessagesStreamMetadataExposesMessageID(t *testing.T) {
	stream := &messagesStream{id: "msg_01AbC"}
	if got := stream.ResponseID(); got != "msg_01AbC" {
		t.Fatalf("ResponseID = %q, want msg_01AbC", got)
	}
	if got := stream.RequestID(); got != "" {
		t.Fatalf("RequestID = %q, want empty", got)
	}
}
