package knowledge

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ChunkSpec is the chunker output: a positionally-tagged content slice
// without dataset/doc identity (the Service fills those in).
type ChunkSpec struct {
	Index   int
	Offset  int
	Content string
}

// Chunker turns raw content into ordered ChunkSpecs.
//
// Implementations MUST be deterministic (same input -> same output) and
// MUST return a stable Sig() so derived data freshness can be checked.
type Chunker interface {
	Split(content string) []ChunkSpec
	Sig() string
}

// NewDefaultChunker returns the built-in paragraph/sentence-boundary
// chunker. Its output is UTF-8 safe; chunks never split inside a
// multi-byte rune.
//
// Sizes are measured in runes (not bytes), so multi-byte UTF-8 content
// (CJK, emoji, etc.) is sliced safely on rune boundaries.
func NewDefaultChunker(cfg ChunkConfig) Chunker {
	return &defaultChunker{cfg: normaliseChunkConfig(cfg)}
}

type defaultChunker struct{ cfg ChunkConfig }

func (c *defaultChunker) Sig() string { return ChunkConfigSig(c.cfg) }

func (c *defaultChunker) Split(content string) []ChunkSpec {
	derived := ChunkText("", content, c.cfg)
	out := make([]ChunkSpec, len(derived))
	for i, d := range derived {
		out[i] = ChunkSpec{Index: d.Index, Offset: d.Offset, Content: d.Content}
	}
	return out
}

// normaliseChunkConfig clamps invalid inputs so downstream code can
// rely on Size > 0 and 0 <= Overlap < Size. Mirrors the legacy
// ChunkDocument behaviour so v0.2.x callers see the same chunks.
func normaliseChunkConfig(c ChunkConfig) ChunkConfig {
	if c.ChunkSize <= 0 {
		c.ChunkSize = 512
	}
	if c.ChunkOverlap < 0 {
		c.ChunkOverlap = 0
	}
	if c.ChunkOverlap >= c.ChunkSize {
		c.ChunkOverlap = c.ChunkSize / 4
	}
	return c
}

// ChunkConfigSig returns a stable signature for a chunker configuration.
// It is the ChunkerSig embedded in DerivedSig so freshness checks can
// detect a configuration change.
func ChunkConfigSig(c ChunkConfig) string {
	n := normaliseChunkConfig(c)
	sum := sha256.Sum256([]byte(fmt.Sprintf("v1|size=%d|overlap=%d", n.ChunkSize, n.ChunkOverlap)))
	return "chunker:" + hex.EncodeToString(sum[:8])
}

// ChunkText splits content into UTF-8-safe overlapping chunks.
//
// Behavior:
//   - Slicing is performed on rune indices, never byte indices.
//   - End offsets are reported in bytes for compatibility with retrieval
//     filters that key on byte position.
//   - Boundary preference: paragraph (\n\n) > sentence (". ") > line (\n).
//   - The result is deterministic for a given (content, cfg) pair.
func ChunkText(docName, content string, cfg ChunkConfig) []DerivedChunk {
	opts := normaliseChunkConfig(cfg)
	content = strings.TrimSpace(content)
	if content == "" {
		return nil
	}

	runes := []rune(content)
	if len(runes) <= opts.ChunkSize {
		return []DerivedChunk{{
			DocName: docName,
			Index:   0,
			Offset:  0,
			Content: content,
		}}
	}

	step := opts.ChunkSize - opts.ChunkOverlap
	if step <= 0 {
		step = 1
	}

	var chunks []DerivedChunk
	for start := 0; start < len(runes); {
		end := start + opts.ChunkSize
		if end > len(runes) {
			end = len(runes)
		}
		if end < len(runes) {
			window := string(runes[start:end])
			if bp := findRuneBreak(window, "\n\n"); bp > 0 {
				end = start + bp
			} else if bp := findRuneBreak(window, ". "); bp > 0 {
				end = start + bp + 1
			} else if bp := findRuneBreak(window, "\n"); bp > 0 {
				end = start + bp
			}
		}
		piece := strings.TrimSpace(string(runes[start:end]))
		if piece != "" {
			chunks = append(chunks, DerivedChunk{
				DocName: docName,
				Index:   len(chunks),
				Offset:  runeOffsetToByteOffset(content, start),
				Content: piece,
			})
		}
		next := end - opts.ChunkOverlap
		if next <= start {
			next = start + 1
		}
		if next >= len(runes) {
			break
		}
		start = next
	}
	return chunks
}

// findRuneBreak finds a separator near the tail (last quarter) of s,
// returning its rune offset within s, or -1 when none is found.
func findRuneBreak(s, sep string) int {
	runes := []rune(s)
	min := len(runes) * 3 / 4
	if min < len(runes)/2 {
		min = len(runes) / 2
	}
	tail := string(runes[min:])
	idx := strings.LastIndex(tail, sep)
	if idx < 0 {
		return -1
	}
	return min + utf8.RuneCountInString(tail[:idx])
}

func runeOffsetToByteOffset(s string, runeIdx int) int {
	if runeIdx <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == runeIdx {
			return i
		}
		count++
	}
	return len(s)
}
