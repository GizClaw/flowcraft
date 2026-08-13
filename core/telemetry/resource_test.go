package telemetry

import (
	"context"
	"testing"
)

func TestBuildResource(t *testing.T) {
	res, err := buildResource(context.Background(), "test-svc", "0.0.1")
	if err != nil {
		t.Fatalf("buildResource error: %v", err)
	}
	if res == nil {
		t.Fatal("expected non-nil resource")
	}

	found := false
	for _, attr := range res.Attributes() {
		if string(attr.Key) == "service.name" && attr.Value.AsString() == "test-svc" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected service.name attribute in resource")
	}
}
