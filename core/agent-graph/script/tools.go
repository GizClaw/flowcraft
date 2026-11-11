package script

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool/engine"
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
	// run and tools count calls so one invocation's records are ordered and its
	// call ids are unique.
	run     int
	calls   int
	records []agent.Record
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
	if h.rt == nil {
		return ToolResult{}, errdefs.Validationf(
			"script: node %q: this kind was built without a runtime, so a script cannot call tools", h.node)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return ToolResult{}, errdefs.Validationf(
			"script: node %q: tool %q arguments: %w", h.node, name, err)
	}
	call := message.ToolCall{
		ID:        h.callID(name),
		Name:      name,
		Arguments: message.NewArguments(raw),
	}
	view, ok := runtime.CatalogViewFrom(ctx)
	if !ok {
		view = h.rt.NewCatalogView()
	}
	result := h.rt.Assembly().Execute(engine.WithCatalogView(ctx, view), call)
	text := result.Content.Text()
	h.record(ToolRecord{
		Name:          name,
		ArgumentsHash: hashArguments(raw),
		Index:         h.calls - 1,
		OK:            !result.IsError,
		Text:          text,
	})
	if result.IsError {
		return ToolResult{}, errdefs.Abortedf(
			"script: node %q: tool %q failed: %s", h.node, name, text)
	}
	return ToolResult{OK: true, Text: text}, nil
}

// record keeps one durable fact for the step's save.
func (h *scriptHost) record(value any) {
	kind := RecordTool
	if _, isTool := value.(ToolRecord); !isTool {
		kind = RecordRun
	}
	h.records = append(h.records, agent.Record{Kind: kind, Payload: payload(value)})
}

// start records the invocation itself, which is what a pinned node reads back to
// know which version its state was produced under.
func (h *scriptHost) start(ref, version string) {
	h.record(RunRecord{Ref: ref, Version: version})
}

// callID names one call for the dispatch chain. It is unique within the
// invocation, which is all the chain needs: the invocation is itself identified by
// the task and step.
func (h *scriptHost) callID(name string) string {
	h.calls++
	return "script:" + strconv.Itoa(h.calls) + ":" + name
}

// hashArguments is the canonical identity of one call's arguments.
func hashArguments(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// hostFor builds the invocation's host.
func hostFor(in graph.Invoke, log io.Writer, rt *runtime.Runtime, allow map[string]struct{}) *scriptHost {
	return &scriptHost{node: in.Node, log: log, rt: rt, allow: allow}
}
