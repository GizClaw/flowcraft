package config

import (
	"context"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

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

// A scheme is a host's own vocabulary: the arguments arrive untouched, and the
// resolver decides what they mean.
func TestExpandDocumentReachesTheResolver(t *testing.T) {
	var seen []string
	expanded, err := ExpandDocument(context.Background(),
		[]byte("token: ${secret:openai:luna}\n"),
		WithResolver("secret", func(_ context.Context, args []string) (string, error) {
			seen = args
			return "resolved", nil
		}),
	)
	if err != nil {
		t.Fatalf("ExpandDocument: %v", err)
	}
	if strings.Join(seen, "/") != "openai/luna" {
		t.Errorf("args = %v, want openai/luna", seen)
	}
	if !strings.Contains(string(expanded), "resolved") {
		t.Errorf("expanded = %q", expanded)
	}
}

// The document is parsed before values are expanded, so a reference inside a
// comment is not a reference.
func TestExpandDocumentIgnoresComments(t *testing.T) {
	expanded, err := ExpandDocument(context.Background(),
		[]byte("# token: ${UNSET_ANYWHERE}\nname: forge\n"))
	if err != nil {
		t.Fatalf("ExpandDocument: %v", err)
	}
	var document yaml.Node
	if err := yaml.Unmarshal(expanded, &document); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
}
