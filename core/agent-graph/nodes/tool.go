package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/runtime"
	"github.com/GizClaw/flowcraft/core/tool/engine"
)

// Tool returns the "tool" node kind: it executes the pending tool calls.
//
// config:
//
//	messages:    state key holding the transcript (type messages), required
//	pending:     state key holding the calls (type json), required
//	effects:     after (default) or before
//	parallelism: how many calls run at once; 0 is unbounded
//
// It appends one tool message with every result and clears the pending list, so a
// cycle back to the model reads both sides of the exchange. The messages key must
// merge as append: the node returns only this step's message, not the transcript.
//
// Calls run in order, one start/result event pair each: the transcript stays
// consistent with the pending list, and a live observer sees which tool is
// running instead of a silent gap between two model rounds.
//
// A tool cannot pause the run: the node has no translation from a tool result
// to graph.Suspend.
type toolKind struct{ rt *runtime.Runtime }

type toolConfig struct {
	Messages string `json:"messages"`
	Pending  string `json:"pending"`
	// Effects is "after" (default) or "before": before writes a started
	// record before the tool runs, so a crash leaves an outcome to reconcile
	// instead of a silent duplicate.
	Effects string `json:"effects"`
	// Parallelism caps how many calls of one batch run at once. 0 leaves the
	// batch unbounded, which is the runtime's default: a host that dispatches
	// to a rate-limited provider should set it.
	Parallelism int `json:"parallelism"`
}

// DecodeConfig parses the node's config at Build.
func (*toolKind) DecodeConfig(raw json.RawMessage) (any, error) {
	return decodeToolConfig(raw)
}

func decodeToolConfig(raw json.RawMessage) (toolConfig, error) {
	var config toolConfig
	if err := graph.DecodeConfig(raw, &config); err != nil {
		return toolConfig{}, err
	}
	return config, nil
}

// Tool builds the tool kind over a runtime.
func Tool(rt *runtime.Runtime) graph.NodeKind { return &toolKind{rt: rt} }

func (*toolKind) Name() string { return "tool" }

func (t *toolKind) Invoke(ctx context.Context, in graph.Invoke) (graph.NodeStream, error) {
	config, err := graph.ConfigOf(in, decodeToolConfig)
	if err != nil {
		return nil, fmt.Errorf("tool: %w", err)
	}
	if strings.TrimSpace(config.Messages) == "" || strings.TrimSpace(config.Pending) == "" {
		return nil, fmt.Errorf("tool: config.messages and config.pending are required")
	}
	if config.Parallelism < 0 {
		return nil, fmt.Errorf("tool: parallelism must not be negative")
	}
	raw, err := in.State(config.Pending).JSON()
	if err != nil {
		return nil, fmt.Errorf("tool: state %q: %w", config.Pending, err)
	}
	var calls []message.ToolCall
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &calls); err != nil {
			return nil, fmt.Errorf("tool: state %q: %w", config.Pending, err)
		}
	}
	if len(calls) == 0 {
		// Nothing to do: a node that finds no work succeeds without writing, so a
		// cycle can route on the state instead of on an error.
		return graph.Static(graph.Result{}), nil
	}

	if _, err := in.State(config.Messages).Messages(); err != nil {
		return nil, fmt.Errorf("tool: state %q: %w", config.Messages, err)
	}
	// The run's projection goes on the context for the duration of the calls:
	// tool_search resolves it from there and the assembly recorder feeds each
	// executed call back into it.
	view, ok := runtime.CatalogViewFrom(ctx)
	if !ok {
		view = t.rt.NewCatalogView()
	}
	effects := strings.TrimSpace(config.Effects)
	switch effects {
	case "", "after":
		effects = "after"
	case "before":
		if in.Checkpoint == nil {
			return nil, fmt.Errorf("tool: effects: before needs a checkpoint")
		}
	default:
		return nil, fmt.Errorf("tool: effects %q is not after or before", config.Effects)
	}
	return &toolStream{
		rt:          t.rt,
		view:        view,
		calls:       calls,
		messagesKey: config.Messages,
		pendingKey:  config.Pending,
		effects:     effects,
		checkpoint:  in.Checkpoint,
		parallelism: config.Parallelism,
	}, nil
}

const recordEffect = "graph.effect"

// toolStream runs the batch concurrently and reports every call: the starts are
// emitted first, then results as they complete. The transcript keeps input order
// regardless of completion order.
type toolStream struct {
	rt          *runtime.Runtime
	view        engine.CatalogView
	calls       []message.ToolCall
	startIndex  int
	launched    bool
	ctx         context.Context
	cancel      context.CancelFunc
	completions chan toolCompletion
	pending     int
	results     []message.ToolResult
	records     [][]graph.Record
	messagesKey string
	pendingKey  string
	effects     string
	checkpoint  func([]graph.Record) error
	parallelism int
	slots       chan struct{}
	result      graph.Result
	done        bool
}

// toolCompletion is one finished call: the event fields and the effect records
// that ride the step's save.
type toolCompletion struct {
	index      int
	result     message.ToolResult
	resultHash string
	duration   time.Duration
	errorKind  string
	records    []graph.Record
}

func (s *toolStream) Next(ctx context.Context) (agent.Event, error) {
	if s.done {
		return nil, io.EOF
	}
	if s.startIndex < len(s.calls) {
		call := s.calls[s.startIndex]
		s.startIndex++
		body, err := json.Marshal(call)
		if err != nil {
			return nil, err
		}
		return agent.ToolCallStart{Call: call, ArgsHash: hashBytes(body)}, nil
	}
	if !s.launched {
		s.launch(ctx)
	}
	if s.pending == 0 {
		s.finish()
		return nil, io.EOF
	}
	select {
	case done := <-s.completions:
		s.pending--
		s.results[done.index] = done.result
		s.records[done.index] = done.records
		return agent.ToolResult{
			Result:     done.result,
			ResultHash: done.resultHash,
			Duration:   done.duration,
			ErrorKind:  done.errorKind,
		}, nil
	case <-s.ctx.Done():
		return nil, s.ctx.Err()
	}
}

// launch starts one goroutine per call. Every call runs at once; the results
// are collected in input order.
func (s *toolStream) launch(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.completions = make(chan toolCompletion, len(s.calls))
	s.pending = len(s.calls)
	s.results = make([]message.ToolResult, len(s.calls))
	s.records = make([][]graph.Record, len(s.calls))
	if s.parallelism > 0 {
		s.slots = make(chan struct{}, s.parallelism)
	}
	for i, call := range s.calls {
		go s.execute(i, call)
	}
	s.launched = true
}

// execute runs one call and reports it. It is the only goroutine touching its
// index's slots; the records it returns are concatenated in input order.
func (s *toolStream) execute(index int, call message.ToolCall) {
	if s.slots != nil {
		select {
		case s.slots <- struct{}{}:
			defer func() { <-s.slots }()
		case <-s.ctx.Done():
			s.completions <- toolCompletion{index: index,
				result: message.NewErrorToolResult(call.ID, s.ctx.Err().Error()), errorKind: "canceled"}
			return
		}
	}
	body, err := json.Marshal(call)
	if err != nil {
		s.completions <- toolCompletion{index: index,
			result: message.NewErrorToolResult(call.ID, err.Error()), errorKind: "encode"}
		return
	}
	argsHash := hashBytes(body)
	var records []graph.Record
	if s.effects == "before" {
		started := agent.EffectRecord{
			CallID: call.ID, ToolName: call.Name, ArgsHash: argsHash,
			IdempotencyKey: call.ID, Status: agent.EffectStarted,
		}
		record, err := effectRecord(started)
		if err != nil {
			s.completions <- toolCompletion{index: index,
				result: message.NewErrorToolResult(call.ID, err.Error()), errorKind: "encode"}
			return
		}
		if err := s.checkpoint([]graph.Record{record}); err != nil {
			s.completions <- toolCompletion{index: index,
				result: message.NewErrorToolResult(call.ID, err.Error()), errorKind: "checkpoint"}
			return
		}
	}
	started := time.Now()
	result := s.rt.Assembly().Execute(engine.WithCatalogView(s.ctx, s.view), call)
	duration := time.Since(started)
	body, err = json.Marshal(result)
	if err != nil {
		s.completions <- toolCompletion{index: index,
			result: message.NewErrorToolResult(call.ID, err.Error()), errorKind: "encode"}
		return
	}
	effect := agent.EffectRecord{
		CallID: call.ID, ToolName: call.Name, ArgsHash: argsHash,
		IdempotencyKey: call.ID, Status: agent.EffectDone,
		ResultHash: hashBytes(body), ErrorKind: toolErrorKind(result),
		DurationMs: duration.Milliseconds(),
	}
	if result.IsError {
		effect.Status = agent.EffectFailed
	}
	record, err := effectRecord(effect)
	if err != nil {
		s.completions <- toolCompletion{index: index, result: result,
			resultHash: effect.ResultHash, duration: duration, errorKind: effect.ErrorKind}
		return
	}
	records = append(records, record)
	s.completions <- toolCompletion{index: index, result: result,
		resultHash: effect.ResultHash, duration: duration,
		errorKind: effect.ErrorKind, records: records}
}

// finish turns the ordered results into the step's writes.
func (s *toolStream) finish() {
	var records []graph.Record
	for _, perCall := range s.records {
		records = append(records, perCall...)
	}
	msg := toolMessage(s.results)
	msg.Metadata = toolOrigin(s.calls)
	s.result = graph.Result{
		State: []graph.StateUpdate{
			{Key: s.messagesKey, Value: graph.MessagesValue(graph.MessagesFromSlice([]message.Message{msg}))},
			{Key: s.pendingKey, Value: graph.JSON([]byte("[]"))},
		},
		Records: records,
	}
	s.done = true
}

func (s *toolStream) Result() (graph.Result, error) {
	if !s.done {
		return graph.Result{}, fmt.Errorf("tool: result before the stream ended")
	}
	return s.result, nil
}

// Close stops calls that are still in flight.
func (s *toolStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// effectRecord renders one effect record.
func effectRecord(effect agent.EffectRecord) (graph.Record, error) {
	payload, err := json.Marshal(effect)
	if err != nil {
		return graph.Record{}, err
	}
	return graph.Record{Kind: recordEffect, Payload: payload}, nil
}

// toolErrorKind classifies a result for the trace. A tool reports failure
// through the result, not by failing the node.
func toolErrorKind(result message.ToolResult) string {
	if result.IsError {
		return "tool_error"
	}
	return ""
}

// toolOrigin tags the tool turn with where it came from: one name when the
// batch is uniform, "tool" when it is mixed.
func toolOrigin(calls []message.ToolCall) *message.Metadata {
	origin := "tool"
	if len(calls) > 0 {
		origin = "tool:" + calls[0].Name
		for _, call := range calls[1:] {
			if call.Name != calls[0].Name {
				origin = "tool"
				break
			}
		}
	}
	raw, err := json.Marshal(origin)
	if err != nil {
		return nil
	}
	return &message.Metadata{Attributes: map[string]json.RawMessage{"origin": raw}}
}

func hashBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// toolMessage wraps results into the one turn a model reads as tool output.
func toolMessage(results []message.ToolResult) message.Message {
	parts := make([]message.Part, 0, len(results))
	for _, result := range results {
		parts = append(parts, message.ToolResultPart{Result: result})
	}
	return message.Message{Role: message.RoleTool, Content: message.Content{Parts: parts}}
}
