package nodes

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/agent/graph"
	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/tool"
	"github.com/goccy/go-json"
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
type toolKind struct{}

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
// Tool builds the tool kind.
//
// It takes nothing: the session's authority and the tool surface arrive with each invocation.
func Tool() graph.NodeKind { return &toolKind{} }

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
	// The binding carries the session's projection: tool_search resolves it from there, and the
	// dispatcher records each executed call back into it.
	view := in.Binding.View
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
		binding:     in.Binding,
		tools:       in.Tools,
		view:        view,
		calls:       calls,
		messagesKey: config.Messages,
		pendingKey:  config.Pending,
		effects:     effects,
		checkpoint:  in.Checkpoint,
		parallelism: config.Parallelism,
	}, nil
}

// toolStream runs the batch concurrently and reports every call: the starts are
// emitted first, then results as they complete. The transcript keeps input order
// regardless of completion order.
type toolStream struct {
	binding     tool.Binding
	tools       tool.Dispatcher
	view        tool.View
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

// launch starts a bounded worker pool: len(calls) workers, lowered to the
// parallelism cap when one is set. A batch of two hundred calls therefore parks
// at most "parallelism" goroutines instead of one per call. The results are
// collected in input order whatever order they complete in.
func (s *toolStream) launch(ctx context.Context) {
	s.ctx, s.cancel = context.WithCancel(ctx)
	s.completions = make(chan toolCompletion, len(s.calls))
	s.pending = len(s.calls)
	s.results = make([]message.ToolResult, len(s.calls))
	s.records = make([][]graph.Record, len(s.calls))
	workers := len(s.calls)
	if s.parallelism > 0 && s.parallelism < workers {
		workers = s.parallelism
	}
	queue := make(chan int)
	for range workers {
		go s.work(queue)
	}
	// One dispatcher, not one sender per call: it leaves the queue instead of
	// parking on a consumer that a cancel has already retired.
	go func() {
		defer close(queue)
		for index := range s.calls {
			select {
			case queue <- index:
			case <-s.ctx.Done():
				return
			}
		}
	}()
	s.launched = true
}

// work pulls call indices until the queue is drained or the batch is canceled,
// running one call at a time: the number of calls in flight is the number of
// workers.
func (s *toolStream) work(queue <-chan int) {
	for {
		select {
		case <-s.ctx.Done():
			return
		case index, ok := <-queue:
			if !ok {
				return
			}
			// The receive can win the race against a done context; a call started
			// after the cancel is work the caller no longer wants.
			if s.ctx.Err() != nil {
				return
			}
			s.execute(index, s.calls[index])
		}
	}
}

// execute runs one call and reports it. Only a worker of this batch touches the
// index's result slot, so the slots are safe to fill out of order; the records
// it returns are concatenated in input order.
func (s *toolStream) execute(index int, call message.ToolCall) {
	body, err := json.Marshal(call)
	if err != nil {
		s.completions <- toolCompletion{index: index,
			result: message.NewErrorToolResult(call.ID, err.Error()), errorKind: "encode"}
		return
	}
	argsHash := hashBytes(body)
	var records []graph.Record
	// The call's start, named once: the "started" record is anchored at it, and the
	// outcome is anchored at the end its duration implies.
	calledAt := time.Now()
	if s.effects == "before" {
		started := agent.EffectRecord{
			CallID: call.ID, ToolName: call.Name, ArgsHash: argsHash,
			IdempotencyKey: call.ID, Status: agent.EffectStarted,
		}
		record, err := effectRecord(started, calledAt)
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
	result := s.tools.Execute(s.ctx, s.binding, call)
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
	record, err := effectRecord(effect, started.Add(duration))
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

// effectRecord renders one effect record, dated at the instant the fact it
// describes happened: the call's start for a "started" record, its end for an
// outcome.
func effectRecord(effect agent.EffectRecord, at time.Time) (graph.Record, error) {
	payload, err := json.Marshal(effect)
	if err != nil {
		return graph.Record{}, err
	}
	return graph.Record{Kind: graph.RecordEffect, Payload: payload, At: at}, nil
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
