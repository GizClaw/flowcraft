package files

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/fsx"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

const (
	// ReadResultChars caps how much text one read result carries; a longer file
	// continues from next_offset.
	ReadResultChars  = 20_000
	defaultReadLimit = 1000
	maxReadLimit     = 100_000
	maxReadFileBytes = 2 << 20
)

type readFileTool struct {
	tree   *fsx.Tree
	root   string
	images bool
}

var _ tool.Tool = (*readFileTool)(nil)

func (t *readFileTool) Definition() message.ToolDefinition {
	return definition(
		ReadFileName,
		"Read a text file and return one window of it. Paths are absolute. "+
			"Returns JSON {file_path, content, offset, limit, total_lines, "+
			"is_truncated, next_offset}: continue from next_offset until "+
			"is_truncated is false.",
		map[string]property{
			"file_path": {kind: "string", desc: "Absolute path of the file to read; defaults to the configured root."},
			"offset":    {kind: "integer", desc: "1-based line to start from.", def: 1, min: num(1)},
			"limit":     {kind: "integer", desc: "Maximum number of lines; fewer may come back when the window is full.", def: defaultReadLimit, min: num(1), max: num(maxReadLimit)},
		},
		"file_path",
	)
}

// describe varies with the wiring: a parameter that always fails is worse than
// an absent one, because the model spends a turn discovering it.
func (t *readFileTool) describe() string {
	text := "Read the contents of a file. Returns the text as-is."
	if t.images {
		return text + " An image returns an image part plus a note with its format and " +
			"size; images whose format this build cannot decode, or whose sides exceed " +
			"4096 px, are refused with an instruction to convert or downscale them."
	}
	return text + " Images are refused: this model does not accept image input."
}

func (t *readFileTool) Metadata() tool.ToolMeta { return tool.ToolMeta{} }

func (t *readFileTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var params struct {
		FilePath string `json:"file_path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
	}
	if err := tool.DecodeArgs(ReadFileName, arguments, nil, &params); err != nil {
		return message.Content{}, err
	}
	path, err := absolute(ReadFileName, params.FilePath, t.root)
	if err != nil {
		return message.Content{}, err
	}
	data, err := t.tree.Read(ctx, path)
	if err != nil {
		return message.Content{}, err
	}
	// An image is not text: return it as an image part, or refuse it by name.
	if detected, isImage := detectImage(data); isImage {
		if !t.images {
			return message.Content{}, validationf(
				"%s: %s is an image (%s) and this model does not accept image input",
				ReadFileName, path, detected.mediaType)
		}
		return imageContent(ReadFileName, path, data, detected)
	}
	if len(data) > maxReadFileBytes {
		return message.Content{}, errdefs.Validationf(
			"%s: %s exceeds the %d-byte limit; use a narrower range",
			ReadFileName, path, maxReadFileBytes)
	}

	total := countLines(data)

	offset := params.Offset
	if offset < 1 {
		offset = 1
	}
	limit := params.Limit
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}
	if offset > total+1 {
		offset = total + 1
	}

	// Only the window is materialized. Building every line first, as splitLines
	// did, made a 20 KB answer walk a file-sized string and a line-sized slice
	// element per line.
	var b strings.Builder
	next := offset
	lineNo := 0
	for at := 0; ; {
		line, after, ok := nextLine(data, at)
		if !ok {
			break
		}
		at = after
		lineNo++
		if lineNo < offset {
			continue
		}
		if lineNo >= offset+limit {
			break
		}
		if b.Len() > 0 && b.Len()+len(line)+1 > ReadResultChars {
			break
		}
		if b.Len() == 0 && utf8.RuneCountInString(string(line)) > ReadResultChars {
			line = []byte(string([]rune(string(line))[:ReadResultChars]))
		}
		b.Write(line)
		b.WriteByte('\n')
		next = lineNo + 1
	}

	return result(ReadFileName, map[string]any{
		"file_path":    path,
		"content":      strings.TrimSuffix(b.String(), "\n"),
		"offset":       offset,
		"limit":        limit,
		"total_lines":  total,
		"is_truncated": next <= total,
		"next_offset":  next,
	})
}

// countLines counts content lines the way splitLines defined them: CRLF is one
// break and a final newline does not open an empty line.
func countLines(data []byte) int {
	total := 0
	for at := 0; ; {
		_, next, ok := nextLine(data, at)
		if !ok {
			return total
		}
		total++
		at = next
	}
}
