package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/GizClaw/flowcraft/core/utils/pathsafe"
)

// The pathsafe dump is committed and compared byte for byte, so it has to be a
// function of core alone. The spot checks at the end are the answers the port
// is most likely to get quietly wrong: `Rel` walking up rather than stripping a
// prefix, `Clean` keeping a relative leading `..` while folding an absolute one,
// and `ResolveUnder` appending an absolute reference before refusing it.

func generatePathsafe(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writePathsafeContract(&buffer); err != nil {
		t.Fatalf("writePathsafeContract: %v", err)
	}
	return buffer.Bytes()
}

func decodePathsafe(t *testing.T) pathsafeContract {
	t.Helper()
	var document pathsafeContract
	if err := json.Unmarshal(generatePathsafe(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestPathsafeContractIsReproducible(t *testing.T) {
	first, second := generatePathsafe(t), generatePathsafe(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

func TestPathsafeContractCoversItsSurface(t *testing.T) {
	document := decodePathsafe(t)
	if len(document.Within) < 20 {
		t.Errorf("within has %d rows; the table is not being walked", len(document.Within))
	}
	if len(document.Rel) < 20 {
		t.Errorf("rel has %d rows", len(document.Rel))
	}
	if len(document.Resolve) < 14 {
		t.Errorf("resolve has %d rows", len(document.Resolve))
	}
	if len(document.RelRef) < 15 {
		t.Errorf("rel_ref has %d rows", len(document.RelRef))
	}
	if len(document.Clean) < 18 {
		t.Errorf("clean has %d rows", len(document.Clean))
	}
	// Rel refuses a mixed pair rather than inventing an answer; a dump where
	// every row succeeded would not be exercising the error path.
	var refusals int
	for _, row := range document.Rel {
		if !row.OK {
			refusals++
			if row.Rel != "" {
				t.Errorf("a refused rel row carries a relative path: %q", row.Rel)
			}
		}
	}
	if refusals == 0 {
		t.Error("no rel row is refused")
	}
}

// The rows are the answers of one legacy and one modern call: filepath.Rel
// (which cleans and walks) and pathsafe.Rel must agree on everything within /
// answers true for, and nothing else. A table can be wrong in this direction
// without either side noticing, so the property is asserted directly.
func TestPathsafeRelAgreesWithFilepathRel(t *testing.T) {
	for _, row := range decodePathsafe(t).Rel {
		// An empty root contains nothing, which is pathsafe's rule and not
		// filepath.Rel's -- the pair is legal for one and not the other.
		if !row.OK || row.Root == "" || row.Target == "" {
			continue
		}
		rel, err := filepath.Rel(row.Root, row.Target)
		if err != nil {
			t.Errorf("filepath.Rel(%q, %q) errored on replay: %v", row.Root, row.Target, err)
			continue
		}
		if rel != row.Rel {
			t.Errorf("filepath.Rel(%q, %q) = %q, pathsafe.Rel says %q",
				row.Root, row.Target, rel, row.Rel)
		}
		if got := pathsafe.Within(row.Root, row.Target); got != !escapes(rel) {
			t.Errorf("within(%q, %q) = %v, rel says %q", row.Root, row.Target, got, rel)
		}
	}
}

func escapes(rel string) bool {
	return rel == ".." || len(rel) > 3 && rel[:3] == ".."+string(filepath.Separator)
}
