package commands

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/GizClaw/flowcraft/core/agent"
	graph "github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
)

// inspectCmd is the "inspect" command: it prints one task's timeline out of the
// checkpoint store.
//
// The reading itself belongs to the engine that wrote the records — it is the only
// layer that knows what a frontier marker or a state write means — so this command
// is the store's path, the newest task when none is named, and the operator's two
// dials: which kinds to show, and how wide a line may be.
func inspectCmd(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	taskID := flags.String("task", "", "task id (default: the newest task)")
	only := flags.String("kind", "", "only facts of one kind, e.g. llm.call")
	width := flags.Int("width", 120, "characters per line; longer lines are clipped")
	if len(args) < 1 {
		return fmt.Errorf("inspect requires a workspace\n\n%s", usage())
	}
	// Flags follow the workspace path, and flag.Parse stops at the first positional
	// argument: parsing the tail is what makes `inspect <ws> --kind x` work.
	workspace := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	return inspectWorkspace(os.Stdout, workspace, *taskID, *only, *width)
}

func inspectWorkspace(w io.Writer, workspace, taskID, only string, width int) error {
	path := filepath.Join(workspace, ".forge", "sessions.db")
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("inspect %s: no session store at %s", workspace, path)
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", workspace, err)
	}
	defer func() { _ = store.Close() }()

	ctx := context.Background()
	if taskID == "" {
		tasks, err := store.List(ctx)
		if err != nil {
			return fmt.Errorf("inspect: list tasks: %w", err)
		}
		if len(tasks) == 0 {
			return fmt.Errorf("inspect %s: no tasks", workspace)
		}
		// List is newest first, and a new turn is a new task: the top one is the
		// turn an operator is looking at.
		taskID = tasks[0].ID
	}
	timeline, err := graph.Read(ctx, store, taskID)
	if err != nil {
		return fmt.Errorf("inspect: %w", err)
	}
	options := []graph.RenderOption{graph.WithWidth(width), graph.WithFormat(scriptSummary)}
	if only != "" {
		options = append(options, graph.WithKinds(only))
	}
	return graph.WriteText(w, timeline, options...)
}

// scriptSummary renders the facts a script node wrote.
//
// The engine's renderer knows the records it wrote and cannot know a node kind it
// was handed, so the demo supplies the one vocabulary its graphs lean on: a rule
// invocation and the tool calls it made. An unknown kind returns false and falls
// back to the raw payload.
func scriptSummary(record agent.EventRecord) (string, bool) {
	switch record.Kind {
	case script.RecordRun:
		var run script.RunRecord
		if json.Unmarshal(record.Payload, &run) != nil {
			return "", false
		}
		mode := "live"
		if run.Replayed {
			mode = "replayed"
		}
		return fmt.Sprintf("%-12s %s  v=%s  %s",
			"script.run", run.Ref, shorten(run.Version, 12), mode), true
	case script.RecordTool:
		var call script.ToolRecord
		if json.Unmarshal(record.Payload, &call) != nil {
			return "", false
		}
		status := "ok"
		if !call.OK {
			status = "failed"
		}
		return fmt.Sprintf("%-12s %s#%d %s  %s  args=%s",
			"script.tool", call.Name, call.Index, status,
			shorten(call.Text, 40), shorten(call.ArgumentsHash, 14)), true
	}
	return "", false
}

// shorten clips a single-line payload field, counting characters so a multi-byte
// answer is not cut in the middle of one.
func shorten(text string, n int) string {
	single := []rune(text)
	if n <= 0 || len(single) <= n {
		return string(single)
	}
	return string(single[:n]) + "…"
}
