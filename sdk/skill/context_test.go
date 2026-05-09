package skill

import (
	"context"
	"testing"
)

func TestWhitelistContextCopiesValues(t *testing.T) {
	in := []string{"github"}
	ctx := WithWhitelist(context.Background(), in)
	in[0] = "mutated"

	got := WhitelistFrom(ctx)
	if len(got) != 1 || got[0] != "github" {
		t.Fatalf("WhitelistFrom() = %#v, want [github]", got)
	}
	got[0] = "changed"
	again := WhitelistFrom(ctx)
	if again[0] != "github" {
		t.Fatalf("WhitelistFrom returned mutable backing slice: %#v", again)
	}
}
