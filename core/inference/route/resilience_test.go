package route

import (
	"testing"

	"github.com/GizClaw/flowcraft/core/inference"
)

func TestNonRetryableKindUndefinedTool(t *testing.T) {
	if !nonRetryableKind(inference.UndefinedTool) {
		t.Fatal("undefined tool rejections must be non-retryable")
	}
	if !nonRetryableKind(inference.InvalidProviderResponse) {
		t.Fatal("invalid provider responses must stay non-retryable")
	}
	if nonRetryableKind(inference.ProviderFailure) {
		t.Fatal("provider failures must remain retryable candidates")
	}
}
