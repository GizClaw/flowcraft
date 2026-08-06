package deploy

import (
	"testing"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
)

func TestResultResourceRejectsTypedNil(t *testing.T) {
	var resource *struct{}
	result := &Result{resources: map[string]any{"nil": resource}}

	if _, err := result.Resource("nil"); err == nil || !errdefs.IsInternal(err) {
		t.Fatalf("Resource(typed nil) error = %v, want internal", err)
	}
}
