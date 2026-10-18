package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GizClaw/flowcraft/core/utils/filetype"
)

// The filetype dump is committed and compared byte for byte, so it has to be a
// function of core alone -- which is why it holds no `NameType` answers: those
// come from the host's MIME database. The checks below keep the two halves
// honest: the sniffer half must reach every signature, and the classify half
// must still be the ordered rule rather than a copy of the sniffer.

func generateFiletype(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if err := writeFiletypeContract(&buffer); err != nil {
		t.Fatalf("writeFiletypeContract: %v", err)
	}
	return buffer.Bytes()
}

func decodeFiletype(t *testing.T) filetypeContract {
	t.Helper()
	var document filetypeContract
	if err := json.Unmarshal(generateFiletype(t), &document); err != nil {
		t.Fatalf("the document does not decode: %v", err)
	}
	return document
}

func TestFiletypeContractIsReproducible(t *testing.T) {
	first, second := generateFiletype(t), generateFiletype(t)
	if !bytes.Equal(first, second) {
		t.Fatal("two dumps of the same inputs differ")
	}
}

func TestFiletypeCorpusReachesEverySignature(t *testing.T) {
	document := decodeFiletype(t)
	if len(document.Sniffs) < 70 {
		t.Fatalf("the corpus has %d rows", len(document.Sniffs))
	}
	types := make(map[string]bool, len(document.Sniffs))
	for _, row := range document.Sniffs {
		types[row.Type] = true
	}
	// One answer per signature family in Go's table, plus the fallback.
	want := []string{
		"text/html; charset=utf-8",
		"text/xml; charset=utf-8",
		"application/pdf",
		"application/postscript",
		"image/x-icon",
		"image/bmp",
		"image/gif",
		"image/webp",
		"image/png",
		"image/jpeg",
		"audio/aiff",
		"audio/mpeg",
		"application/ogg",
		"audio/midi",
		"video/avi",
		"audio/wave",
		"video/mp4",
		"video/webm",
		"application/vnd.ms-fontobject",
		"font/ttf",
		"font/otf",
		"font/collection",
		"font/woff",
		"font/woff2",
		"application/x-gzip",
		"application/zip",
		"application/x-rar-compressed",
		"application/wasm",
		"text/plain; charset=utf-8",
		"text/plain; charset=utf-16be",
		"text/plain; charset=utf-16le",
		"application/octet-stream",
	}
	for _, type_ := range want {
		if !types[type_] {
			t.Errorf("the corpus never reaches %q", type_)
		}
	}
}

// The classify half is filetype's rule, not the sniffer's answer: a text
// payload under a sniffer-only signature reads as text, and the charset in a
// sniffer answer never reaches a Kind.
func TestFiletypeClassifyIsTheOrderedRule(t *testing.T) {
	document := decodeFiletype(t)
	if len(document.Classify) != len(document.Sniffs) {
		t.Fatalf("classify has %d rows, sniffs has %d", len(document.Classify), len(document.Sniffs))
	}
	for _, row := range document.Classify {
		if strings.Contains(row.MediaType, ";") {
			t.Errorf("a Kind carries parameters: %q", row.MediaType)
		}
		if row.Text && row.MediaType != "text/plain" && !strings.HasPrefix(row.MediaType, "text/") {
			t.Errorf("a text payload is named %q", row.MediaType)
		}
		if got := filetype.IsText(row.Data); got != row.Text {
			t.Errorf("IsText(%x) = %v, the row says %v", row.Data, got, row.Text)
		}
		if got := filetype.OfData("blob", row.Data); got.MediaType != row.MediaType {
			t.Errorf("OfData(%x) = %q, the dump says %q", row.Data, got.MediaType, row.MediaType)
		}
	}
}

// The UTF-16 marks are the sample that separates "trim the answer" from "keep
// it": the sniffer names a charset, the classifier drops it, and the payload is
// binary either way.
func TestFiletypeUtf16MarkLosesItsCharset(t *testing.T) {
	data := []byte{0xFE, 0xFF, 0x00, 0x00}
	if got := http.DetectContentType(data); got != "text/plain; charset=utf-16be" {
		t.Fatalf("the sniffer answered %q", got)
	}
	kind := filetype.OfData("blob", data)
	if kind.MediaType != "text/plain" || kind.Text {
		t.Fatalf("OfData = %+v, want text/plain and not text", kind)
	}
}
