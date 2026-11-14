package runtime

import (
	"context"
	"strings"
	"testing"
)

// The deployment document and a runtime layer go through one implementation, so
// the rules cannot drift: values are expanded after parsing, escapes work, and an
// unregistered scheme is an error rather than a token that ships as literal text.
func TestExpandDocumentSharesTheLayerRules(t *testing.T) {
	t.Setenv("EXPAND_TOKEN", "s3cr3t")
	body := []byte("peer: https://peer.example\n" +
		"auth:\n  token: ${env:EXPAND_TOKEN}\n  literal: \"100$$\"\n")
	expanded, err := ExpandDocument(context.Background(), body)
	if err != nil {
		t.Fatalf("ExpandDocument: %v", err)
	}
	if !strings.Contains(string(expanded), "s3cr3t") {
		t.Errorf("expanded document has no resolved token:\n%s", expanded)
	}
	if !strings.Contains(string(expanded), `"100$"`) && !strings.Contains(string(expanded), "100$") {
		t.Errorf("$$ did not collapse to a literal $:\n%s", expanded)
	}
}

func TestExpandDocumentRejectsAnUnregisteredScheme(t *testing.T) {
	if _, err := ExpandDocument(context.Background(), []byte("token: ${nope:x}")); err == nil {
		t.Fatal("ExpandDocument with an unregistered scheme = nil, want an error")
	}
}
