package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// The media dump is committed and compared byte for byte, so it has to be a
// function of core alone, and its rows have to stay samples of the calls they
// claim to be. The spot checks at the end are the ones worth writing down
// twice: behavior that reads like a bug report until you check which path
// refuses it.

func generateMessage(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := writeMessageContract(&buf); err != nil {
		t.Fatalf("writeMessageContract: %v", err)
	}
	return buf.Bytes()
}

func decodeMessage(t *testing.T) messageContract {
	t.Helper()
	var doc messageContract
	if err := json.Unmarshal(generateMessage(t), &doc); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return doc
}

func TestMessageContractIsReproducible(t *testing.T) {
	first, second := generateMessage(t), generateMessage(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two generations differ; the document must be a function of core alone")
	}
}

func TestMessageContractRowsCarryTheirInput(t *testing.T) {
	doc := decodeMessage(t)
	if len(doc.MediaSources) == 0 || len(doc.AudioFormats) == 0 {
		t.Fatal("a dump without rows is not a contract")
	}
	for _, row := range doc.MediaSources {
		where := row.Target + "/" + row.Op
		switch row.Op {
		case "new_inline":
			// Either the payload or the type is named: an empty payload and an
			// empty type are samples of their own rules, but a row that names
			// neither is not a call anybody could replay.
			if row.Bytes == "" && row.MediaType == "" {
				t.Errorf("%s: a row names its payload or its media type", where)
			}
		case "new_url":
			if row.URL == "" {
				t.Errorf("%s: a url row needs its url", where)
			}
		case "from_wire":
			if row.Document == "" {
				t.Errorf("%s: a document row needs its document", where)
			}
		default:
			t.Errorf("%s: unknown op", where)
		}
		if row.Outcome != "ok" && row.Outcome != "error" {
			t.Errorf("%s: outcome %q is neither", where, row.Outcome)
		}
		if row.Outcome == "error" && row.Error == "" {
			t.Errorf("%s: a refusal without its sentence cannot be replayed", where)
		}
		if row.Outcome == "ok" && row.Wire == "" {
			t.Errorf("%s: an accepted source without its wire form cannot be compared", where)
		}
		if row.TextFrom != "" && row.Op != "from_wire" {
			t.Errorf("%s: only the decoder's refusals are foreign text", where)
		}
	}
	for _, row := range doc.AudioFormats {
		if row.Outcome == "error" && row.Error == "" {
			t.Errorf("%s: a refusal without its sentence cannot be replayed", row.Encoding)
		}
		if row.MediaType == "" {
			t.Errorf("%s: every encoding in the table maps to a media type", row.Encoding)
		}
	}
}

// TestMessageContractMediaTypesAreTyped checks the typed half of the
// constructors' promise on every accepted row: what came out is the family the
// target asked for.
func TestMessageContractMediaTypesAreTyped(t *testing.T) {
	doc := decodeMessage(t)
	families := map[string]string{"image": "image/", "audio": "audio/", "video": "video/"}
	for _, row := range doc.MediaSources {
		if row.Outcome != "ok" {
			continue
		}
		prefix := families[row.Target]
		if row.MediaTypeOut != "" && !strings.HasPrefix(row.MediaTypeOut, prefix) {
			t.Errorf("%s: accepted %q, which is not %s", row.MediaType, row.MediaTypeOut, prefix)
		}
		if row.BaseMediaType != "" && !strings.HasPrefix(row.BaseMediaType, prefix) {
			t.Errorf("%s: base type %q is not %s", row.MediaType, row.BaseMediaType, prefix)
		}
	}
}

func findMedia(t *testing.T, doc messageContract, what string, match func(mediaRow) bool) mediaRow {
	t.Helper()
	var found []mediaRow
	for _, row := range doc.MediaSources {
		if match(row) {
			found = append(found, row)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%s: expected one row, found %d", what, len(found))
	}
	return found[0]
}

// TestMessageContractPinsTheAsymmetries records the behavior that differs
// between the constructors and the typed decode, and between what reads like a
// validation rule and what the validation says.
func TestMessageContractPinsTheAsymmetries(t *testing.T) {
	doc := decodeMessage(t)

	// A fragment-bearing url: what url.ParseRequestURI refuses is a missing
	// scheme or host, not a fragment.
	fragment := findMedia(t, doc, "fragment url", func(row mediaRow) bool {
		return row.Op == "new_url" && strings.Contains(row.URL, "#")
	})
	if fragment.Outcome != "ok" {
		t.Errorf("fragment url: core refused it with %q", fragment.Error)
	}

	// A relative url through the typed decode: the document path checks the
	// payload, not the URI, so a document may carry what a constructor would
	// not build.
	relative := findMedia(t, doc, "relative url document", func(row mediaRow) bool {
		return row.Op == "from_wire" && strings.Contains(row.Document, `"/relative"`)
	})
	if relative.Outcome != "ok" {
		t.Errorf("relative url document: core refused it with %q", relative.Error)
	}

	// A trailing semicolon is not a parameter: ParseMediaType drops it.
	trailing := findMedia(t, doc, "trailing semicolon", func(row mediaRow) bool {
		return row.MediaType == "IMAGE/PNG;"
	})
	if trailing.Outcome != "ok" || trailing.MediaTypeOut != "image/png" {
		t.Errorf("trailing semicolon: got %q with %q", trailing.Outcome, trailing.MediaTypeOut)
	}

	// Zeros. The raw families require positive numbers; the compressed ones
	// read zero as "not declared", so one family refuses what the other reads
	// as the metadata it does not have.
	zeros := map[string]string{}
	for _, row := range doc.AudioFormats {
		if (row.SampleRateHz == nil || *row.SampleRateHz != 0) && (row.Channels == nil || *row.Channels != 0) {
			continue
		}
		zeros[row.Encoding] = row.Outcome
	}
	for _, raw := range []string{"pcm16", "pcm24", "float32"} {
		if outcome, ok := zeros[raw]; ok && outcome != "error" {
			t.Errorf("%s with a zero: raw formats require positive numbers", raw)
		}
	}
	for _, compressed := range []string{"mp3", "opus"} {
		if outcome, ok := zeros[compressed]; ok && outcome != "ok" {
			t.Errorf("%s with a zero: compressed formats read zero as absent", compressed)
		}
	}
	// One row per family: two for the raw side, two for the compressed side,
	// and the raw pair collapses onto one family name.
	if len(zeros) < 3 {
		t.Errorf("the zero cases are the interesting ones and there are only %d", len(zeros))
	}
}
