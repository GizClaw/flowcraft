package filetype

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pngBytes is a real PNG header: the signature carries no NUL, the IHDR
// chunk's dimensions do.
func pngBytes() []byte {
	return []byte{
		0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A,
		0x00, 0x00, 0x00, 0x0D, 'I', 'H', 'D', 'R',
		0x00, 0x00, 0x00, 0x02, 0x00, 0x00, 0x00, 0x02,
		0x08, 0x06, 0x00, 0x00, 0x00,
	}
}

// jpegBytes starts with the SOI marker and an APP0 segment.
func jpegBytes() []byte {
	return append(
		[]byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00},
		bytes.Repeat([]byte{0x11}, 16)...,
	)
}

// mp4Bytes carries the ftyp box the WHATWG sniffer matches, so the
// content names video/mp4 on every platform.
func mp4Bytes() []byte {
	data := []byte{0x00, 0x00, 0x00, 0x20}
	data = append(data, []byte("ftypisom")...)
	data = append(data, 0x00, 0x00, 0x00, 0x00)
	data = append(data, []byte("mp41")...)
	return append(data, bytes.Repeat([]byte{0x42}, 32)...)
}

// tsPacket is one PAT packet of an MPEG transport stream: sync byte,
// PID 0, table_id 0, then NUL padding — what a .ts recording is.
func tsPacket() []byte {
	return append([]byte{0x47, 0x40, 0x00, 0x10, 0x00}, make([]byte, 183)...)
}

func TestOfPathDecidesByContentNotName(t *testing.T) {
	source := "export const answer = 42;\n"
	cases := []struct {
		name     string
		data     []byte
		wantType string
		wantText bool
	}{
		{
			// The case that motivated the content rule: the platform
			// table maps .ts to video/mp2t, the bytes are TypeScript.
			name: "app.ts", data: []byte(source), wantText: true,
		},
		{
			name: "module.mts", data: []byte(source), wantText: true,
		},
		{
			// The same extension, opposite answer: a transport stream.
			// The exact type follows the platform table (video/mp2t
			// where it knows .ts, octet-stream where it does not), so
			// only the text verdict is pinned.
			name: "recording.ts", data: tsPacket(),
		},
		{
			// A video name cannot make text a video.
			name: "clip.mp4", data: []byte("not a video at all\n"),
			wantText: true,
		},
		{
			// A text name cannot hide a picture: the bytes win.
			name: "screenshot.txt", data: pngBytes(), wantType: "image/png",
		},
		{
			// Nor can a text name hide a video.
			name: "notes.md", data: mp4Bytes(), wantType: "video/mp4",
		},
		{
			// A wrong media name cannot beat the bytes either.
			name: "photo.png", data: jpegBytes(), wantType: "image/jpeg",
		},
		{
			// Two-letter WHATWG patterns that documents start with:
			// "BM" is image/bmp, "ID3" is audio/mpeg.
			name: "bm25.md", data: []byte("BM25 ranks documents by term frequency.\n"),
			wantText: true,
		},
		{
			name: "tags.md", data: []byte("ID3 tags carry track metadata.\n"),
			wantText: true,
		},
		{
			// The pattern is real when the bytes carry it: a BMP header
			// plus the size fields that make a payload binary.
			name:     "icon.bmp",
			data:     append([]byte("BM"), make([]byte, 56)...),
			wantType: "image/bmp",
		},
		{
			// A text-only PDF preamble has no NUL for the sniffer to lean
			// on, so the name corroborates the signature.
			name: "report.pdf", data: []byte("%PDF-1.4 fake\n"),
			wantType: "application/pdf",
		},
		{
			// Text under a name the table calls an image: the payload is
			// text, so the file opens as text.
			name: "diagram.svg", data: []byte("<svg viewBox=\"0 0 1 1\"></svg>\n"),
			wantText: true,
		},
		{
			// JSON, YAML, TOML and friends are text whatever the table
			// calls them.
			name: "config.json", data: []byte("{\"a\": 1}\n"), wantText: true,
		},
		{
			// A NUL byte rules text out even under a text name: the bytes
			// are as specific as the answer gets.
			name: "broken.md", data: []byte{0x00, 0x01, 0x02, 0x7F},
		},
		{
			// An empty file is text, so an empty source file still opens
			// in the code viewer.
			name: "empty.go", data: nil, wantText: true,
		},
		{
			// No extension at all: the bytes decide between text and the
			// generic binary fallback.
			name: "LICENSE", data: []byte("MIT License\n"), wantText: true,
		},
		{
			name: "blob", data: []byte{0x00, 0x01, 0x02, 0x03},
		},
		{
			// An archive whose exact format only the name knows.
			name: "report.docx", data: append([]byte("PK\x03\x04"), make([]byte, 24)...),
			wantType: "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, tc.data, 0o644); err != nil {
				t.Fatal(err)
			}
			got := OfPath(path)
			if got.Text != tc.wantText {
				t.Errorf("text = %v, want %v (type %q)",
					got.Text, tc.wantText, got.MediaType)
			}
			if tc.wantType != "" && got.MediaType != tc.wantType {
				t.Errorf("media type = %q, want %q", got.MediaType, tc.wantType)
			}
			if got.Text && Family(got.MediaType) != "" {
				t.Errorf("text classified as %q, want no media family",
					got.MediaType)
			}
			if got.MediaType == "" {
				t.Error("media type is empty; the classifier always names one")
			}
		})
	}
}

// TestOfPathWithoutReadableContent pins the name-only fallback: a path
// that is not a readable file still classifies, so callers listing a
// directory do not need a special case.
func TestOfPathWithoutReadableContent(t *testing.T) {
	dir := t.TempDir()
	if got := OfPath(dir); Family(got.MediaType) != "" {
		t.Errorf("directory = %+v, want no media family", got)
	}
	if got := OfPath(filepath.Join(dir, "missing.txt")); !got.Text {
		t.Errorf("missing path = %+v, want the text fallback", got)
	}
}

// TestOfDataMatchesOfPath pins that the in-memory form answers for the
// same bytes the same way the path form does, so a tool that already
// holds a payload (a sandbox read, a download) classifies files exactly
// like the viewer.
func TestOfDataMatchesOfPath(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"app.ts", []byte("export const answer = 42;\n")},
		{"clip.mp4", []byte("not a video at all\n")},
		{"screenshot.txt", pngBytes()},
		{"recording.ts", tsPacket()},
		{"blob", []byte{0x00, 0x01, 0x02}},
	} {
		path := filepath.Join(dir, tc.name)
		if err := os.WriteFile(path, tc.data, 0o644); err != nil {
			t.Fatal(err)
		}
		if got, want := OfData(tc.name, tc.data), OfPath(path); got != want {
			t.Errorf("%s: OfData = %+v, OfPath = %+v", tc.name, got, want)
		}
	}
}

func TestIsTextStopsAtTheFirstNUL(t *testing.T) {
	if !IsText(nil) || !IsText([]byte("plain text\n")) {
		t.Error("text without a NUL byte must read as text")
	}
	if IsText([]byte("head\x00tail")) {
		t.Error("a NUL byte anywhere rules text out")
	}
}

func TestNameTypeTrimsParameters(t *testing.T) {
	if got := NameType("note.txt"); got != "text/plain" {
		t.Errorf("NameType(note.txt) = %q, want text/plain", got)
	}
	if got := NameType("doc.unknown-extension"); got != "" {
		t.Errorf("NameType of an unlisted extension = %q, want empty", got)
	}
}

func TestFamily(t *testing.T) {
	for mediaType, want := range map[string]string{
		"image/png":            "image",
		"video/mp2t":           "video",
		"audio/mpeg":           "audio",
		"application/pdf":      "pdf",
		"text/plain":           "",
		"application/zip":      "",
		"application/pdf+xml?": "",
	} {
		if got := Family(mediaType); got != want {
			t.Errorf("Family(%q) = %q, want %q", mediaType, got, want)
		}
	}
	if !strings.HasPrefix(NameType("index.html"), "text/") {
		t.Errorf("NameType(index.html) = %q, want a text type",
			NameType("index.html"))
	}
}
