// Package filetype classifies a file by its content.
//
// Hosts render, stage and archive files by their media type, so the
// name alone is not enough evidence: macOS and Linux map .ts to
// video/mp2t, which would stream TypeScript source into a <video>
// element, and no table tells a screenshot saved as .txt from a note.
// The rule below decides for every file, whatever the platform's
// extension table says, and every caller that needs a file's type uses
// it.
//
// Classification, in order:
//
//  1. A media signature in the leading bytes (image, video, audio, PDF)
//     wins when the payload is binary or the name agrees on the family.
//     The corroboration guards the two-letter patterns of the WHATWG
//     table ("BM" is image/bmp, "ID3" is audio/mpeg), which ordinary
//     documents can start with; the name keeps those text. A real image,
//     video or container carries a NUL byte in the sample (sizes, counts
//     and reserved fields are small numbers), while a PDF with a text
//     preamble may not, so a .pdf still matches through its name.
//  2. A payload without a NUL byte is text, whatever the name says: a
//     .ts recording is the transport stream its table entry describes,
//     but the TypeScript source beside it opens as source. The name may
//     only refine the subtype (text/csv, text/markdown).
//  3. Anything else is binary and takes the name's type when the table
//     has one (.mkv, .heic, .docx, .zip carry no signature the sample
//     can name), and the sniffed type otherwise.
//
// Text has one definition here: a payload is text exactly when it has no
// NUL byte. Encodings that embed NULs (UTF-16) therefore read as binary.
package filetype

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// sniffLen is how many leading bytes the classifier samples.
// net/http's DetectContentType (the WHATWG mimesniff algorithm, section
// 5) looks at no more than this.
const sniffLen = 512

// Kind is how a host should treat one file: the media type to report
// for it and whether its payload is text.
type Kind struct {
	// MediaType is the type to report; it is never empty.
	MediaType string
	// Text reports whether the payload is text, so a viewer can render it
	// as source and a tool can read it as lines.
	Text bool
}

// OfPath classifies the file at path. A path the process cannot read as
// a file (a directory, a permission error) is classified by its name
// alone, with an empty payload that reads as text.
func OfPath(path string) Kind {
	head, err := readHead(path)
	if err != nil {
		return classify(path, nil)
	}
	return classify(path, head)
}

// OfData classifies a payload the caller already holds: a workspace file
// read through the sandbox, a download a tool just fetched. name
// supplies the extension and nothing else. The sample is the same
// sniffLen the path form reads, so a file classifies identically either
// way; a caller holding the whole payload may additionally apply IsText
// to it.
func OfData(name string, data []byte) Kind {
	sample := data
	if len(sample) > sniffLen {
		sample = sample[:sniffLen]
	}
	return classify(name, sample)
}

// IsText reports whether a payload is text under the rule the viewer,
// the attachments and the tools share: text carries no NUL byte.
// OfPath applies it to the sampled head; a caller holding the whole
// payload can apply it to the full bytes, which can only demote a text
// sample to binary.
func IsText(data []byte) bool {
	return bytes.IndexByte(data, 0) < 0
}

// Family returns the rendering family of a media type: "image",
// "video", "audio", "pdf", or "" for everything a host hands to an
// external application.
func Family(mediaType string) string {
	switch {
	case strings.HasPrefix(mediaType, "image/"):
		return "image"
	case strings.HasPrefix(mediaType, "video/"):
		return "video"
	case strings.HasPrefix(mediaType, "audio/"):
		return "audio"
	case mediaType == "application/pdf":
		return "pdf"
	default:
		return ""
	}
}

// NameType returns the MIME type the platform's extension table maps
// name to, trimmed of its parameters, or "" when the table does not know
// the extension. It is the naming half of the classification above.
func NameType(name string) string {
	return trimmedMediaType(mime.TypeByExtension(filepath.Ext(name)))
}

// classify implements the ordered rule in the package comment.
func classify(name string, head []byte) Kind {
	table := NameType(name)
	sample := trimmedMediaType(http.DetectContentType(head))
	if family := Family(sample); family != "" &&
		(!IsText(head) || Family(table) == family) {
		return Kind{MediaType: sample}
	}
	if IsText(head) {
		if strings.HasPrefix(table, "text/") {
			return Kind{MediaType: table, Text: true}
		}
		return Kind{MediaType: "text/plain", Text: true}
	}
	if table != "" && !strings.HasPrefix(table, "text/") {
		return Kind{MediaType: table}
	}
	// A binary payload no name claims, or one whose name claims text: the
	// bytes are as specific as the answer gets.
	return Kind{MediaType: sample}
}

// readHead samples the leading bytes the classifier reads. Short files
// return what they have; a path that cannot be opened as a file (a
// directory) returns an error.
func readHead(path string) (head []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}()
	head = make([]byte, sniffLen)
	n, err := io.ReadFull(f, head)
	if err != nil && !errors.Is(err, io.EOF) &&
		!errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, fmt.Errorf("filetype: read %q: %w", path, err)
	}
	return head[:n], nil
}

// trimmedMediaType drops the parameters a table entry or the sniffer
// attaches ("text/plain; charset=utf-8").
func trimmedMediaType(mediaType string) string {
	if i := strings.IndexByte(mediaType, ';'); i >= 0 {
		mediaType = mediaType[:i]
	}
	return strings.TrimSpace(mediaType)
}
