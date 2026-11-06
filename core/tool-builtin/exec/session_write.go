package exec

import (
	"context"

	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
)

// writeStdinTool feeds a running session and returns what it produced since the
// cursor. Sending and reading are one action because a model that writes without
// reading has no way to know whether the process answered.
type writeStdinTool struct{ group *SessionGroup }

var _ tool.Tool = (*writeStdinTool)(nil)

func (t *writeStdinTool) Definition() message.ToolDefinition {
	return definition(WriteStdinName,
		"Send input to a running session and return its output after a cursor. "+
			"Pass close_stdin to signal end of input. Returns JSON "+
			"{process_id, chunks:[{seq, stream, data}], next_seq, eof}.",
		map[string]any{
			"process_id": map[string]any{"type": "string", "description": "Session id."},
			"data":       map[string]any{"type": "string", "description": "Bytes for stdin; omit to only read."},
			"close_stdin": map[string]any{"type": "boolean", "default": false,
				"description": "Close stdin after writing, so a reader sees EOF."},
			"after_seq": map[string]any{"type": "integer", "minimum": 0, "default": 0,
				"description": "Output cursor from a previous read."},
			"max_bytes": map[string]any{"type": "integer", "default": defaultRead, "minimum": 1, "maximum": maxReadBytes,
				"description": "Bytes to return."},
		},
		"process_id",
	)
}

func (t *writeStdinTool) Metadata() tool.ToolMeta { return tool.ToolMeta{MutatesState: true} }

func (t *writeStdinTool) Execute(ctx context.Context, arguments string) (message.Content, error) {
	var a sessionArgs
	if err := tool.DecodeArgs(WriteStdinName, arguments, nil, &a); err != nil {
		return message.Content{}, err
	}
	session, err := t.group.get(a.ProcessID)
	if err != nil {
		return message.Content{}, err
	}
	if a.Data != "" {
		if err := session.Write(ctx, []byte(a.Data)); err != nil {
			return message.Content{}, err
		}
	}
	if a.CloseStdin {
		if err := session.CloseInput(); err != nil {
			return message.Content{}, err
		}
	}
	payload, err := t.group.read(ctx, a)
	if err != nil {
		return message.Content{}, err
	}
	return encode(WriteStdinName, payload)
}
