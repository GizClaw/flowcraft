package storage

import (
	"context"
	"strings"
	"testing"
)

// TestWorkspaceKVSupportsLongIdentifiers guards the single-layer encoding:
// caller-encoded segments must not be re-encoded, otherwise a 100-character
// conversation id overflows the 255-byte filename limit.
func TestWorkspaceKVSupportsLongIdentifiers(t *testing.T) {
	store, err := NewWorkspaceKV(newTestWorkspace(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id := strings.Repeat("conversation-", 10) // 130 characters
	key := "views/summary/v1/records/" + EncodeSegment(id) + ".json"
	if err := store.Put(ctx, key, []byte("value")); err != nil {
		t.Fatalf("put long identifier: %v", err)
	}
	entries, err := store.List(ctx, "views/summary/v1/records")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Key != key {
		t.Fatalf("list = %#v", entries)
	}
}

// TestNameToPathRejectsOversizedSegments pins the fail-fast behavior for
// identifiers that cannot fit a portable path segment.
func TestNameToPathRejectsOversizedSegments(t *testing.T) {
	tooLong := strings.Repeat("x", 300)
	if _, err := nameToPath("root", "views/"+EncodeSegment(tooLong)); err == nil {
		t.Fatal("expected an explicit length error")
	}
}

// TestPathToNameAcceptsWindowsWalkSeparators guards the module against a
// workspace adapter whose Walk still returns platform separators (older core
// on Windows). Storage names are slash-separated, so a backslash can only be
// a host separator and must be normalized, not rejected.
func TestPathToNameAcceptsWindowsWalkSeparators(t *testing.T) {
	root := "storage/v1/log"
	const want = "streams/" + "stream"
	encoded, err := nameToPath(root, want)
	if err != nil {
		t.Fatalf("nameToPath: %v", err)
	}
	// Simulate a Walk result from a workspace adapter that still reports the
	// platform separator on Windows.
	windowsPath := strings.ReplaceAll(encoded, "/", `\`)
	name, err := pathToName(root, windowsPath)
	if err != nil {
		t.Fatalf("pathToName: %v", err)
	}
	if name != want {
		t.Fatalf("name = %q, want %q", name, want)
	}
}

// TestPathSegmentEncodingRoundTrips pins injectivity of the name-to-path
// mapping, including the boundary between canonical and literal segments.
func TestPathSegmentEncodingRoundTrips(t *testing.T) {
	values := []string{
		"views", "fact", "conversation-1", "k_ABC", "e_ABC", "k_", " ", "ünïcode",
	}
	seen := make(map[string]string, len(values))
	for _, value := range values {
		segment := encodePathSegment(value)
		decoded, err := decodePathSegment(segment)
		if err != nil {
			t.Fatalf("decode %q (%q): %v", value, segment, err)
		}
		if decoded != value {
			t.Fatalf("round trip %q -> %q -> %q", value, segment, decoded)
		}
		if previous, exists := seen[segment]; exists {
			t.Fatalf("collision between %q and %q", previous, value)
		}
		seen[segment] = value
	}
	// A canonical caller-encoded segment must pass through unchanged so the
	// total expansion stays at one base32 layer.
	canonical := EncodeSegment("conversation-1")
	if got := encodePathSegment(canonical); got != canonical {
		t.Fatalf("canonical segment changed: %q -> %q", canonical, got)
	}
}
