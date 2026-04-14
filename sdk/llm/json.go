package llm

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractMeta describes where JSON was extracted from.
type ExtractMeta struct {
	// FromCodeBlock is true if extraction was performed inside a fenced code block.
	FromCodeBlock bool
	// CodeBlockLang is the language tag of the code block (lowercased), if any.
	CodeBlockLang string

	// SearchStart/SearchEnd delimit the slice of the original text we searched within.
	SearchStart int
	SearchEnd   int

	// Start/End delimit the extracted JSON value within the original text (byte offsets).
	Start int
	End   int
}

// ExtractJSON extracts the first complete JSON value (object/array/scalar) from text.
// It tolerates surrounding prose and markdown fenced code blocks, preferring
// a ```json block when present.
func ExtractJSON(text string) ([]byte, ExtractMeta, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ExtractMeta{}, fmt.Errorf("empty input")
	}

	searchText := text
	searchStart := 0
	meta := ExtractMeta{
		SearchStart: 0,
		SearchEnd:   len(text),
	}

	var fenceBlock codeBlock
	var hasFence bool

	if block, ok := findFencedCodeBlock(text, true); ok {
		fenceBlock = block
		hasFence = true
	} else if block, ok := findFencedCodeBlock(text, false); ok {
		fenceBlock = block
		hasFence = true
	}

	if hasFence {
		searchText = fenceBlock.Content
		searchStart = fenceBlock.Start
		meta.FromCodeBlock = true
		meta.CodeBlockLang = fenceBlock.Lang
		meta.SearchStart = fenceBlock.Start
		meta.SearchEnd = fenceBlock.End

		// For ```json fences we trust the content fully (scalars included).
		// For other languages we only look for structured values inside the
		// fence — a ```python block containing "hello" should not match.
		structuredOnly := fenceBlock.Lang != "json"

		startRel, endRel, err := scanFirstJSONValue(searchText, structuredOnly)
		if err == nil {
			meta.Start = searchStart + startRel
			meta.End = searchStart + endRel
			return []byte(text[meta.Start:meta.End]), meta, nil
		}

		// Fence contained no usable JSON — fall back to full text scan.
		meta.FromCodeBlock = false
		meta.CodeBlockLang = ""
		meta.SearchStart = 0
		meta.SearchEnd = len(text)
		searchText = text
		searchStart = 0
	}

	startRel, endRel, err := scanFirstJSONValue(searchText, false)
	if err != nil {
		return nil, meta, err
	}
	meta.Start = searchStart + startRel
	meta.End = searchStart + endRel
	return []byte(text[meta.Start:meta.End]), meta, nil
}

// candidate holds a successfully parsed JSON value position.
type candidate struct {
	start int
	end   int
	found bool
}

// scanFirstJSONValue scans s for the first JSON value with structured values
// (object/array) taking priority over scalars (string/number/bool/null).
// When structuredOnly is true, scalar values are ignored entirely.
func scanFirstJSONValue(s string, structuredOnly bool) (start int, end int, _ error) {
	b := []byte(s)
	var structured, scalar candidate

	for i := 0; i < len(b); i++ {
		c := b[i]
		if isSpaceByte(c) {
			continue
		}
		if !isJSONStartByte(c) {
			continue
		}

		switch {
		case c == '{' || c == '[':
			if structured.found {
				continue
			}
			si, ei, ok := tryDecode(b, i)
			if !ok {
				continue
			}
			structured = candidate{si, ei, true}
			return structured.start, structured.end, nil

		case c == '"':
			if structuredOnly || scalar.found {
				continue
			}
			si, ei, ok := tryDecode(b, i)
			if ok {
				scalar = candidate{si, ei, true}
			}

		case c >= '0' && c <= '9':
			if structuredOnly || scalar.found {
				continue
			}
			si, ei, ok := tryDecode(b, i)
			if ok {
				scalar = candidate{si, ei, true}
			}

		case c == '-':
			if structuredOnly || scalar.found {
				continue
			}
			if i+1 < len(b) && b[i+1] >= '0' && b[i+1] <= '9' {
				si, ei, ok := tryDecode(b, i)
				if ok {
					scalar = candidate{si, ei, true}
				}
			}

		case isKeywordStart(c):
			if structuredOnly || scalar.found {
				continue
			}
			kw := keywordAt(b, i)
			if kw == "" {
				continue
			}
			scalar = candidate{i, i + len(kw), true}
			i += len(kw) - 1
		}
	}

	if structured.found {
		return structured.start, structured.end, nil
	}
	if scalar.found {
		return scalar.start, scalar.end, nil
	}
	return 0, 0, fmt.Errorf("no JSON value found")
}

func tryDecode(b []byte, i int) (start, end int, ok bool) {
	dec := json.NewDecoder(bytes.NewReader(b[i:]))
	dec.UseNumber()
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return 0, 0, false
	}
	consumed := int(dec.InputOffset())
	if consumed <= 0 {
		return 0, 0, false
	}
	return i, i + consumed, true
}

func isSpaceByte(c byte) bool {
	switch c {
	case ' ', '\n', '\r', '\t', '\v', '\f':
		return true
	default:
		return false
	}
}

func isJSONStartByte(c byte) bool {
	if c == '{' || c == '[' || c == '"' || c == '-' {
		return true
	}
	if c >= '0' && c <= '9' {
		return true
	}
	return isKeywordStart(c)
}

func isKeywordStart(c byte) bool {
	return c == 't' || c == 'f' || c == 'n'
}

// keywordAt checks whether b[i:] starts with a JSON keyword (true/false/null)
// followed by a non-alpha byte (or EOF). Returns the matched keyword or "".
func keywordAt(b []byte, i int) string {
	for _, kw := range []string{"true", "false", "null"} {
		if i+len(kw) > len(b) {
			continue
		}
		if string(b[i:i+len(kw)]) != kw {
			continue
		}
		if i+len(kw) < len(b) && isAlpha(b[i+len(kw)]) {
			continue
		}
		return kw
	}
	return ""
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

// codeBlock represents the content of a markdown fenced code block.
type codeBlock struct {
	Content string
	Start   int // byte offset of content start within original text
	End     int // byte offset of content end within original text
	Lang    string
}

// findFencedCodeBlock finds a markdown fenced code block and returns its inner content.
// If preferJSON is true, it returns the first block whose language is "json" (case-insensitive).
// Otherwise it returns the first block found.
func findFencedCodeBlock(text string, preferJSON bool) (codeBlock, bool) {
	const fence = "```"
	var first codeBlock
	var firstFound bool

	searchFrom := 0
	for {
		open := strings.Index(text[searchFrom:], fence)
		if open == -1 {
			break
		}
		open += searchFrom

		lineEnd := strings.IndexByte(text[open+len(fence):], '\n')
		if lineEnd == -1 {
			searchFrom = open + len(fence)
			continue
		}
		lineEnd = open + len(fence) + lineEnd
		tag := strings.TrimSpace(text[open+len(fence) : lineEnd])
		tag = strings.ToLower(tag)

		contentStart := lineEnd + 1
		close := strings.Index(text[contentStart:], fence)
		if close == -1 {
			break
		}
		close += contentStart

		contentEnd := close
		// Trim trailing \r\n or \n before closing fence.
		if contentEnd > contentStart && text[contentEnd-1] == '\n' {
			contentEnd--
		}
		if contentEnd > contentStart && text[contentEnd-1] == '\r' {
			contentEnd--
		}

		b := codeBlock{
			Content: text[contentStart:contentEnd],
			Start:   contentStart,
			End:     contentEnd,
			Lang:    tag,
		}
		if !firstFound {
			first = b
			firstFound = true
		}
		if preferJSON && tag == "json" {
			return b, true
		}

		searchFrom = close + len(fence)
	}

	if firstFound && !preferJSON {
		return first, true
	}
	return codeBlock{}, false
}
