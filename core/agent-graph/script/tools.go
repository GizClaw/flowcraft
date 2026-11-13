package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool/engine"
	"github.com/goccy/go-json"
)

// scriptHost is the [Host] one invocation sees.
//
// It owns the three policies a backend must not implement itself: which tools the
// config allows, what a tool call records, and how a failure is classified. A
// second language reuses all three by calling the same interface.
type scriptHost struct {
	node  string
	log   io.Writer
	rt    *runtime.Runtime
	allow map[string]struct{}
	// replay answers a tool call from the task's records instead of dispatching.
	replay bool
	// recorded reads those records back by kind.
	recorded func(kind string) []agent.Record
	// calls counts invocations so one run's records are ordered and its call ids
	// are unique.
	calls int
	// records are this invocation's facts, in order.
	records []agent.Record
	// index is the replay view of the task's tool records, built on first use.
	index map[string]ToolRecord
}

var _ Host = (*scriptHost)(nil)

// Log implements [Host].
func (h *scriptHost) Log(level LogLevel, msg string) {
	if h.log == nil {
		return
	}
	if h.node != "" {
		fmt.Fprintf(h.log, "[%s] script node %q: %s\n", level, h.node, msg)
		return
	}
	fmt.Fprintf(h.log, "[%s] script: %s\n", level, msg)
}

// Tool implements [Host].
//
// The allow list is the whole capability policy: a name the config did not
// declare is refused before any dispatch, so a script can never reach a tool the
// document did not name — including one it guessed. Everything after that is the
// runtime's ordinary path: the same middleware chain, the same policy checks and
// the same catalog view a tool node uses.
func (h *scriptHost) Tool(ctx context.Context, name string, args map[string]any) (ToolResult, error) {
	if _, allowed := h.allow[name]; !allowed {
		return ToolResult{}, errdefs.Forbiddenf(
			"script: node %q: tool %q is not in config.allow", h.node, name)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolResult{}, errdefs.Validationf(
			"script: node %q: tool %q arguments: %w", h.node, name, err)
	}
	index := h.calls
	h.calls++
	hash := hashArguments(raw)

	if h.replay {
		record, ok := h.lookup(hash, index)
		if !ok {
			return ToolResult{}, errdefs.Abortedf(
				"script: node %q: replay has no record of tool %q call %d", h.node, name, index)
		}
		if !record.OK {
			return ToolResult{}, errdefs.Abortedf(
				"script: node %q: tool %q failed: %s", h.node, name, record.Text)
		}
		return ToolResult{OK: true, Text: record.Text}, nil
	}

	if h.rt == nil {
		return ToolResult{}, errdefs.Validationf(
			"script: node %q: this kind was built without a runtime, so a script cannot call tools", h.node)
	}
	call := message.ToolCall{
		ID:        callID(h.node, name, index),
		Name:      name,
		Arguments: message.NewArguments(raw),
	}
	view, ok := runtime.CatalogViewFrom(ctx)
	if !ok {
		view = h.rt.NewCatalogView()
	}
	started := time.Now()
	result := h.rt.Assembly().Execute(engine.WithCatalogView(ctx, view), call)
	text := result.Content.Text()
	h.recordAt(started, ToolRecord{
		Name:          name,
		ArgumentsHash: hash,
		Index:         index,
		OK:            !result.IsError,
		Text:          text,
	})
	if result.IsError {
		return ToolResult{}, errdefs.Abortedf(
			"script: node %q: tool %q failed: %s", h.node, name, text)
	}
	return ToolResult{OK: true, Text: text}, nil
}

// lookup finds the record a replay answers with.
//
// The index is built once per invocation from the task's own records: a script
// makes a handful of calls, so scanning them is cheaper than keeping a structure
// the engine would have to key.
func (h *scriptHost) lookup(hash string, index int) (ToolRecord, bool) {
	if h.index == nil {
		h.index = make(map[string]ToolRecord)
		if h.recorded != nil {
			for _, record := range h.recorded(RecordTool) {
				var call ToolRecord
				if err := json.Unmarshal(record.Payload, &call); err != nil {
					continue
				}
				h.index[recordKey(call.ArgumentsHash, call.Index)] = call
			}
		}
	}
	call, ok := h.index[recordKey(hash, index)]
	return call, ok
}

// recordKey identifies one call by what it asked and where it sat in the run.
func recordKey(hash string, index int) string {
	return hash + "#" + strconv.Itoa(index)
}

// callID names one call for the dispatch chain.
func callID(node, name string, index int) string {
	return "script:" + strconv.Itoa(index) + ":" + name
}

// recordAt keeps one durable fact for the step's save, dated where the fact it
// describes began: a tool call is anchored at its start, the way a model call is.
//
// A replay records nothing: the facts it answers from are already in the task, and
// appending them again would double the log the replay exists to read.
func (h *scriptHost) recordAt(at time.Time, value any) {
	if h.replay {
		return
	}
	kind := RecordTool
	if _, isTool := value.(ToolRecord); !isTool {
		kind = RecordRun
	}
	h.records = append(h.records, agent.Record{
		Kind:    kind,
		Payload: payload(value),
		At:      at,
	})
}

// start records the invocation itself, which is what a pinned node reads back to
// know which version its state was produced under.
//
// It records even under replay: a replay answers the tool calls from the task's
// own records and writes none, so this line is the only trace that a step was
// answered rather than run, and a log that cannot say so reads like a live call.
func (h *scriptHost) start(ref, version string) {
	value := RunRecord{Ref: ref, Version: version, Replayed: h.replay}
	h.records = append(h.records, agent.Record{
		Kind:    RecordRun,
		Payload: payload(value),
		At:      time.Now(),
	})
}

// hashArguments is the canonical identity of one call's arguments.
func hashArguments(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hostFor builds the invocation's host.
func hostFor(in graph.Invoke, log io.Writer, rt *runtime.Runtime, allow map[string]struct{}) *scriptHost {
	return &scriptHost{
		node:     in.Node,
		log:      log,
		rt:       rt,
		allow:    allow,
		replay:   in.Replay,
		recorded: in.Records,
	}
}
