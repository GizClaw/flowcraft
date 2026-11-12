package commands

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	graph "github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent/checkpoint"
)

// inspectCmd is the "inspect" command: it prints one task's timeline out of the
// checkpoint store.
//
// It exists because the store is the only place that knows what a run did: the
// engine writes a step marker, the state each key held, the rule that ran and every
// tool call. Reading that back by hand means joining three tables and knowing the
// per-task sequence numbering, which is exactly the work this command does.
func inspectCmd(args []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	taskID := flags.String("task", "", "task id (default: the newest task)")
	only := flags.String("kind", "", "only facts of one kind, e.g. graph.frontier")
	limit := flags.Int("limit", 120, "characters of a tool answer or state value to print")
	if len(args) < 1 {
		return fmt.Errorf("inspect requires a workspace\n\n%s", usage())
	}
	// Flags follow the workspace path, and flag.Parse stops at the first positional
	// argument: parsing the tail is what makes `inspect <ws> --kind x` work.
	workspace := args[0]
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	return inspectWorkspace(os.Stdout, workspace, *taskID, *only, *limit)
}

func inspectWorkspace(w io.Writer, workspace, taskID, only string, limit int) error {
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
	events, err := store.Events(ctx, taskID)
	if err != nil {
		return fmt.Errorf("inspect: events: %w", err)
	}
	fmt.Fprintf(w, "task %s  (%d facts)\n", taskID, len(events))
	printTimeline(w, events, only, limit)
	return nil
}

// printTimeline writes one line per fact, grouped under the step marker that
// precedes it.
func printTimeline(w io.Writer, events []agent.EventRecord, only string, limit int) {
	// A document key is printed as the fields this step changed, not the whole
	// document: a game's state is stable across most steps, and repeating it hides
	// the one field a step actually wrote.
	previous := make(map[string]map[string]string)
	for _, event := range events {
		if only != "" && event.Kind != only {
			continue
		}
		switch event.Kind {
		case graph.RecordFrontier:
			var mark struct {
				Step   int      `json:"step"`
				Nodes  []string `json:"nodes"`
				Replay bool     `json:"replay"`
			}
			if json.Unmarshal(event.Payload, &mark) != nil {
				continue
			}
			mode := "live"
			if mark.Replay {
				mode = "replayed"
			}
			fmt.Fprintf(w, "\nstep %d  %s  (%s)\n", mark.Step, strings.Join(mark.Nodes, ", "), mode)
		case graph.RecordState:
			var write struct {
				Step  int             `json:"step"`
				Key   string          `json:"key"`
				Value json.RawMessage `json:"value"`
			}
			if json.Unmarshal(event.Payload, &write) != nil {
				continue
			}
			fields, ok := documentFields(write.Value)
			if !ok {
				fmt.Fprintf(w, "  state %-8s %s\n", write.Key, summarizeState(write.Value, limit))
				continue
			}
			before := previous[write.Key]
			previous[write.Key] = fields
			changed := make([]string, 0, len(fields))
			for name, value := range fields {
				if before == nil || before[name] != value {
					changed = append(changed, name+"="+value)
				}
			}
			sort.Strings(changed)
			if len(changed) == 0 {
				continue
			}
			sort.SliceStable(changed, func(i, j int) bool { return false })
			line := strings.Join(changed, " ")
			if len(line) > limit*3 {
				line = line[:limit*3] + fmt.Sprintf("… (%d fields)", len(changed))
			}
			fmt.Fprintf(w, "  state %-8s %s\n", write.Key, line)
		default:
			fmt.Fprintf(w, "  %-16s %s\n", event.Kind, summarizeRecord(event.Payload, limit))
		}
	}
}

// summarizeRecord renders the payloads this workspace's deployments write. Every
// engine names its own facts, so an unknown payload prints clipped rather than
// disappearing.
func summarizeRecord(payload json.RawMessage, limit int) string {
	var run struct {
		Ref      string `json:"ref"`
		Version  string `json:"version"`
		Replayed bool   `json:"replayed"`
	}
	if json.Unmarshal(payload, &run) == nil && run.Ref != "" {
		mode := "live"
		if run.Replayed {
			mode = "replayed"
		}
		return fmt.Sprintf("%s  v=%s  %s", run.Ref, clip(run.Version, 12), mode)
	}
	var call struct {
		Name          string `json:"name"`
		Index         int    `json:"index"`
		OK            bool   `json:"ok"`
		ArgumentsHash string `json:"arguments_hash"`
		Text          string `json:"text"`
	}
	if json.Unmarshal(payload, &call) == nil && call.Name != "" {
		status := "ok"
		if !call.OK {
			status = "failed"
		}
		return fmt.Sprintf("%s#%d %s  %s  args=%s",
			call.Name, call.Index, status, clip(oneLine(call.Text), limit), clip(call.ArgumentsHash, 14))
	}
	return clip(oneLine(string(payload)), limit)
}

// summarizeState prints the value a key held. A JSON document is summarised by its
// scalar fields, because a game's state is otherwise hundreds of characters per
// step and the question is almost always "which phase, who is alive".
func summarizeState(value json.RawMessage, limit int) string {
	var encoded struct {
		Kind     string          `json:"kind"`
		Text     string          `json:"text"`
		Bool     bool            `json:"bool"`
		JSON     json.RawMessage `json:"json"`
		Messages []struct {
			Role string `json:"role"`
			Text string `json:"text"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(value, &encoded); err != nil {
		return clip(oneLine(string(value)), limit)
	}
	switch encoded.Kind {
	case "text":
		return clip(oneLine(encoded.Text), limit)
	case "bool":
		return fmt.Sprintf("%t", encoded.Bool)
	case "messages":
		if len(encoded.Messages) == 0 {
			return "(no messages)"
		}
		last := encoded.Messages[len(encoded.Messages)-1]
		return fmt.Sprintf("%d messages, last %s: %s",
			len(encoded.Messages), last.Role, clip(oneLine(last.Text), limit))
	case "json":
		fields, ok := documentFields(value)
		if !ok {
			return clip(oneLine(string(encoded.JSON)), limit)
		}
		keys := make([]string, 0, len(fields))
		for key := range fields {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+fields[key])
		}
		return clip(strings.Join(parts, " "), limit*2)
	}
	return clip(oneLine(string(value)), limit)
}

// documentFields renders one JSON document as field → value, with collections
// reduced to their size: "phase=day day=2 alive=8" is what a diff can compare.
func documentFields(value json.RawMessage) (map[string]string, bool) {
	var encoded struct {
		Kind string          `json:"kind"`
		JSON json.RawMessage `json:"json"`
	}
	if err := json.Unmarshal(value, &encoded); err != nil || encoded.Kind != "json" {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded.JSON, &fields); err != nil {
		return nil, false
	}
	out := make(map[string]string, len(fields))
	for key, raw := range fields {
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) == nil && strings.HasPrefix(strings.TrimSpace(string(raw)), "[") {
			out[key] = fmt.Sprintf("%d items", len(list))
			continue
		}
		out[key] = scalar(raw)
	}
	return out, true
}

// scalar renders one document field, keeping lists short and strings unquoted.
func scalar(raw json.RawMessage) string {
	text := oneLine(string(raw))
	if strings.HasPrefix(text, "\"") {
		var value string
		if json.Unmarshal(raw, &value) == nil {
			return value
		}
	}
	if len(text) > 24 {
		text = text[:24] + "…"
	}
	return text
}

func oneLine(text string) string {
	text = strings.ReplaceAll(text, "\r", " ")
	text = strings.ReplaceAll(text, "\n", " ⏎ ")
	return strings.Join(strings.Fields(text), " ")
}

func clip(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	return text[:limit] + "…"
}
