package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// Invoke is one node execution.
type Invoke struct {
	// Node is the id the graph gave this instance.
	Node string
	// Config is the node's own settings as written.
	Config json.RawMessage
	// Decoded is Config after the kind decoded it at Build. It is nil when the
	// kind has no decoder, and when a caller invoked the kind without Build.
	Decoded any
	// State reads a declared state key as it was when the step started. It is a
	// function, not a map, so a node cannot hold a stale copy across steps.
	State func(key string) Value
	// Snapshot is the whole state of the step, for a node that evaluates an
	// expression over it (a condition) rather than reading named keys. It is a
	// read-only view: the step's map is shared by every node running in it, so
	// handing out the map itself would let one node change what another sees.
	Snapshot func() Snapshot
	// Step is the loop counter, so a node can decide to stop.
	Step int
	// Answer is the reply to a question this node asked by suspending. It is
	// non-empty only on the invocation that resumes a suspended run, and the
	// node is re-entered from the start: anything it did before suspending is
	// its own idempotency problem.
	Answer message.Content
	// Lookup returns a model call this run recorded earlier, by request hash.
	// Nil when nothing was recorded; a node must not assume replay is on.
	Lookup func(requestHash string) (agent.LLMCallRecord, bool)
	// Replay is true when the run should answer from Lookup instead of calling
	// out. A miss is an error, not a silent live call.
	Replay bool
	// Trace is how much of the run is traced. A node that computes input
	// digests for the checkpoint only does so when this allows TraceNode or
	// Replay is on.
	Trace telemetry.TraceLevel
	// Bodies keeps the bytes of a recorded response. It is a retention decision,
	// not a level: a recorded call without them is a call that can be attributed
	// but not replayed.
	Bodies bool
	// Records returns the durable facts earlier steps of this task recorded under
	// one kind, oldest first. A node that replays reads its own record kind: an
	// empty slice means nothing was recorded, which is a first run rather than an
	// error, so the caller decides what a miss means.
	Records func(kind string) []Record
	// Checkpoint writes records immediately, for a side effect that must be
	// recorded before it runs. Nil when the run has no store.
	Checkpoint func(records []Record) error
}

// Record is one durable fact a node produced; the engine persists it with the
// step's save.
type Record = agent.Record

// Result is one node's outcome.
//
// A node produces state and nothing else: there are no output ports, because an
// edge carries no value. What the node computed becomes state, and the edges
// leaving it decide where the run goes next.
type Result struct {
	// State is what the node wants to change for the next step. A node that
	// suspends loses these writes: it is re-entered on resume and produces them
	// again, so committing them now would apply them twice.
	State []StateUpdate
	// Suspend asks the run to stop and wait for the caller. The step finishes
	// first, so the suspension cannot tear a concurrent step apart.
	Suspend *Suspend
	// Records are durable facts this invocation produced: a model call, an
	// input digest, a side effect. The engine writes them with the step's save.
	Records []Record
}

// Suspend is a node asking the caller a question.
//
// It is a primitive, not a node kind: asking is not a computation, and any node
// may ask — a tool node relaying an ask_user tool, a custom node collecting a
// missing parameter. One node may suspend per step.
type Suspend struct {
	Question message.Content
}

// NodeKind is one node type.
//
// It declares a name and how to run, and nothing else: a kind is a host
// extension point, not an external contract. Writes are declared by the state
// keys the node returns, and a condition that does not match its state is a run
// error with the node named.
type NodeKind interface {
	// Name is the document's kind: "model", "tool", "condition", ...
	Name() string
	// Invoke opens one node execution.
	//
	// The node returns a stream rather than calling back into the engine: the
	// engine pulls events as the caller is ready for them, so backpressure and
	// cancellation are the pull contract rather than a convention, and a kind
	// can be tested by feeding it inputs and reading the stream.
	Invoke(ctx context.Context, in Invoke) (NodeStream, error)
}

// Snapshot is a read-only view of one step's state.
type Snapshot struct {
	values map[string]Value
}

// NewSnapshot wraps a map as a snapshot. The engine does this for every step; a
// test that invokes a kind directly does the same.
func NewSnapshot(values map[string]Value) Snapshot { return Snapshot{values: values} }

// Get returns one value, or the zero Value when the key is not in the state.
func (s Snapshot) Get(key string) Value { return s.values[key] }

// Range calls visit for every entry; visit returns false to stop.
func (s Snapshot) Range(visit func(key string, value Value) bool) {
	for key, value := range s.values {
		if !visit(key, value) {
			return
		}
	}
}

// Len reports how many keys the snapshot holds.
func (s Snapshot) Len() int { return len(s.values) }

// ConfigDecoder lets a kind parse its config once, at Build, so a run does not
// decode the same document on every invocation.
type ConfigDecoder interface {
	DecodeConfig(raw json.RawMessage) (any, error)
}

// ConfigOf returns the config Build decoded, or decodes the raw document for a
// caller that invoked the kind directly (a test).
func ConfigOf[T any](in Invoke, decode func(json.RawMessage) (T, error)) (T, error) {
	if typed, ok := in.Decoded.(T); ok {
		return typed, nil
	}
	return decode(in.Config)
}

// NodeStream is one node execution in flight.
//
// Next follows the pull contract: io.EOF ends it, any other error fails the
// node, and the caller then calls Result — or Close, if it stopped early. A node
// that produces no events returns [Static].
//
// The engine pulls one stream per node, so Next is never called concurrently
// with itself. Events a node emits interleave with those of other nodes in the
// same step; the order within one node is the order it produced them.
type NodeStream interface {
	// Next returns the next event, or io.EOF when the node is done.
	Next(ctx context.Context) (agent.Event, error)
	// Result is the node's outcome, valid once Next reported io.EOF.
	Result() (Result, error)
	// Close releases whatever the node opened.
	Close() error
}

// Static wraps a finished Result as a stream that emits nothing.
//
// Most kinds compute and return; only a kind that wants to report progress (a
// model call streaming text, a long tool run) needs its own stream.
func Static(result Result) NodeStream { return &staticStream{result: result} }

type staticStream struct {
	result Result
	done   bool
}

func (s *staticStream) Next(context.Context) (agent.Event, error) {
	s.done = true
	return nil, io.EOF
}

func (s *staticStream) Result() (Result, error) {
	if !s.done {
		return Result{}, fmt.Errorf("graph: Result before the stream ended")
	}
	return s.result, nil
}

func (*staticStream) Close() error { return nil }

// Registry is the table of node kinds. Its shape matches the engine tables in
// core/deploy: a kind is a name resolved at build time, so an unknown kind is a
// document error with the known names in the message.
type Registry struct {
	kinds map[string]NodeKind
}

// NewRegistry returns a registry holding kinds. Duplicate names are an error.
func NewRegistry(kinds ...NodeKind) (*Registry, error) {
	registry := &Registry{kinds: make(map[string]NodeKind, len(kinds))}
	for _, kind := range kinds {
		if err := registry.Register(kind); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

// Register adds one kind.
func (r *Registry) Register(kind NodeKind) error {
	if kind == nil {
		return fmt.Errorf("graph: a nil node kind")
	}
	name := kind.Name()
	if name == "" {
		return fmt.Errorf("graph: a node kind without a name")
	}
	if _, exists := r.kinds[name]; exists {
		return fmt.Errorf("graph: node kind %q is already registered", name)
	}
	r.kinds[name] = kind
	return nil
}

// Lookup returns one kind by name.
func (r *Registry) Lookup(name string) (NodeKind, bool) {
	kind, ok := r.kinds[name]
	return kind, ok
}
