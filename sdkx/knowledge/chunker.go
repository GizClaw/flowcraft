package knowledge

import "strings"

// ChunkDocument splits content into overlapping chunks, preferring to
// break at paragraph or sentence boundaries.
func ChunkDocument(docName, content string, cfg ChunkConfig) []Chunk {
	if cfg.ChunkSize <= 0 {
		cfg.ChunkSize = 512
	}
	if cfg.ChunkOverlap < 0 {
		cfg.ChunkOverlap = 0
	}
	if cfg.ChunkOverlap >= cfg.ChunkSize {
		cfg.ChunkOverlap = cfg.ChunkSize / 4
	}

	content = strings.TrimSpace(content)
	if len(content) == 0 {
		return nil
	}
	if len(content) <= cfg.ChunkSize {
		return []Chunk{{DocName: docName, Index: 0, Content: content, Offset: 0}}
	}

	var chunks []Chunk
	step := cfg.ChunkSize - cfg.ChunkOverlap
	if step <= 0 {
		step = 1
	}

	for offset := 0; offset < len(content); {
		end := offset + cfg.ChunkSize
		if end > len(content) {
			end = len(content)
		}

		// Try to break at paragraph boundary
		if end < len(content) {
			if bp := findBreak(content[offset:end], "\n\n"); bp > 0 {
				end = offset + bp
			} else if bp := findBreak(content[offset:end], ". "); bp > 0 {
				end = offset + bp + 1 // include the period
			} else if bp := findBreak(content[offset:end], "\n"); bp > 0 {
				end = offset + bp
			}
		}

		chunk := strings.TrimSpace(content[offset:end])
		if chunk != "" {
			chunks = append(chunks, Chunk{
				DocName: docName,
				Index:   len(chunks),
				Content: chunk,
				Offset:  offset,
			})
		}

		nextOffset := offset + (end - offset)
		if nextOffset <= offset {
			nextOffset = offset + step
		}
		// Apply overlap
		nextOffset -= cfg.ChunkOverlap
		if nextOffset <= offset {
			nextOffset = offset + 1
		}
		if nextOffset >= len(content) {
			break
		}
		offset = nextOffset
	}
	return chunks
}

// findBreak returns the position of the last occurrence of sep in s,
// searching only the last quarter of the string (to keep chunks reasonably sized).
func findBreak(s, sep string) int {
	minPos := len(s) * 3 / 4
	if minPos < len(s)/2 {
		minPos = len(s) / 2
	}
	idx := strings.LastIndex(s[minPos:], sep)
	if idx < 0 {
		return -1
	}
	return minPos + idx
}
