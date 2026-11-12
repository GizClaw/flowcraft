package agenttrace

import (
	"context"
	"encoding/json"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/agent-graph"
	"github.com/GizClaw/flowcraft/core/agent-graph/script"
	"github.com/GizClaw/flowcraft/core/errdefs"
	"github.com/GizClaw/flowcraft/core/message"
)

// Decoder turns one record's payload into the type the engine that wrote it
// defines. False means "not one of mine": the reader tries the next decoder and
// then keeps the fact opaque.
//
// The type is a plain function so a decoder lives beside the encoder it mirrors
// without that package having to import this one.
type Decoder func(agent.EventRecord) (any, bool)

// DecodeMessage decodes the `message` records a protocol adapter appends: a
// message that entered the task, in the core shape.
func DecodeMessage(record agent.EventRecord) (any, bool) {
	if record.Kind != "message" {
		return nil, false
	}
	var decoded message.Message
	if json.Unmarshal(record.Payload, &decoded) != nil {
		return nil, false
	}
	return decoded, true
}

// Fact is one record, as the log ordered and dated it.
type Fact struct {
	// Seq is the store's monotonic sequence within the task.
	Seq int64
	// At is when the fact happened; zero means unknown (a row written before
	// events carried a time).
	At time.Time
	// Kind is the engine's vocabulary: graph.frontier, llm.call, script.tool.
	Kind string
	// Step is the frontier the fact belongs to. Facts written before the first
	// marker belong to step 0: the input the run was seeded with.
	Step int
	// Payload is the record's bytes, always kept so an opaque fact is still
	// readable.
	Payload json.RawMessage
	// Value is what a Decoder made of the payload, nil when none claimed it.
	Value any
}

// Step is one frontier: the nodes that ran, and the facts they left behind.
type Step struct {
	// Ordinal is the 1-based step the marker names; 0 is the seeded input, which
	// no frontier ran.
	Ordinal int
	// At is when the step's records were written.
	At time.Time
	// Nodes are the frontier's node ids, in declaration order.
	Nodes []string
	// Replay reports that the step answered from records: no model was called.
	Replay bool
	// Facts are the records this step wrote, ordered.
	Facts []Fact
}

// Trace is one task read back: its standing, its version combination, and every
// fact the log holds, ordered and dated.
type Trace struct {
	Task        agent.Task
	Fingerprint agent.Fingerprint
	// Manifest is the version combination the run executed under, empty for a
	// task written before the engine recorded one.
	Manifest agent.Manifest
	// Projection is where the graph engine stood: its step, its frontier, and
	// the hash of the decision point.
	Projection graph.Projection
	// Steps groups the facts by frontier. Facts recorded before any marker form
	// the first step, ordinal 0.
	Steps []Step
	// Facts is the whole log, ordered by sequence.
	Facts []Fact

	store agent.Store
}

// Option configures one read.
type Option func(*config)

type config struct {
	decoders []Decoder
	after    int64
	limit    int
}

// WithDecoders replaces the decoders this read uses. The defaults claim the
// graph engine's records, a script node's records, and the messages a protocol
// adapter appends; a host with its own engine passes its decoder here.
func WithDecoders(decoders ...Decoder) Option {
	return func(c *config) { c.decoders = decoders }
}

// WithAfter reads only the facts recorded after seq, which is what a reader
// following a run incrementally asks for.
func WithAfter(seq int64) Option {
	return func(c *config) { c.after = seq }
}

// WithLimit stops after n facts. Zero reads all of them.
func WithLimit(n int) Option {
	return func(c *config) { c.limit = n }
}

// DefaultDecoders are the decoders a read uses when it is given none.
func DefaultDecoders() []Decoder {
	return []Decoder{graph.DecodeRecord, script.DecodeRecord, DecodeMessage}
}

// Read loads one task and returns its timeline.
//
// The task's identity and its sections come from the projection; the facts come
// from the event log; neither is a substitute for the other. A task the store
// does not hold reports [agent.ErrNoCheckpoint].
func Read(ctx context.Context, store agent.Store, taskID string, opts ...Option) (*Trace, error) {
	if store == nil {
		return nil, errdefs.Validationf("agenttrace: a store is required")
	}
	settings := config{decoders: DefaultDecoders()}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	checkpoint, err := store.Load(ctx, taskID)
	if err != nil {
		return nil, err
	}
	records, err := store.EventsAfter(ctx, taskID, settings.after)
	if err != nil {
		return nil, err
	}
	if settings.limit > 0 && len(records) > settings.limit {
		records = records[:settings.limit]
	}
	trace := &Trace{
		Task:        checkpoint.Task,
		Fingerprint: checkpoint.Fingerprint,
		store:       store,
	}
	if raw, ok := checkpoint.Sections[graph.SectionGraph]; ok {
		if projection, err := graph.DecodeProjection(raw); err == nil {
			trace.Projection = projection
		}
	}
	if raw, ok := checkpoint.Sections[graph.SectionManifest]; ok {
		var manifest agent.Manifest
		if json.Unmarshal(raw, &manifest) == nil {
			trace.Manifest = manifest
		}
	}
	trace.assemble(records, settings.decoders)
	return trace, nil
}

// ReadSession reads the newest task of one session, which is what a CLI asked
// for "the last run" needs.
func ReadSession(ctx context.Context, store agent.Store, sessionID string, opts ...Option) (*Trace, error) {
	if store == nil {
		return nil, errdefs.Validationf("agenttrace: a store is required")
	}
	checkpoint, err := store.LatestForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return Read(ctx, store, checkpoint.Task.ID, opts...)
}

// assemble decodes the records and groups them by frontier.
//
// A marker introduces a step and belongs to it: the engine writes the marker
// first, so every fact after it is that step's. Facts recorded before the first
// marker — the input a durable run seeds — form step 0.
func (t *Trace) assemble(records []agent.EventRecord, decoders []Decoder) {
	t.Facts = make([]Fact, 0, len(records))
	for _, record := range records {
		fact := Fact{
			Seq:     record.Seq,
			At:      record.At,
			Kind:    record.Kind,
			Payload: record.Payload,
			Value:   decode(record, decoders),
		}
		if frontier, ok := fact.Value.(graph.FrontierRecord); ok {
			t.Steps = append(t.Steps, Step{
				Ordinal: frontier.Step,
				At:      record.At,
				Nodes:   frontier.Nodes,
				Replay:  frontier.Replay,
			})
		} else if len(t.Steps) == 0 {
			t.Steps = append(t.Steps, Step{At: record.At})
		}
		fact.Step = t.Steps[len(t.Steps)-1].Ordinal
		t.Steps[len(t.Steps)-1].Facts = append(t.Steps[len(t.Steps)-1].Facts, fact)
		t.Facts = append(t.Facts, fact)
	}
}

// decode asks each decoder in turn, and keeps the fact opaque when none claims
// it — a foreign engine's record is not an error.
func decode(record agent.EventRecord, decoders []Decoder) any {
	for _, decoder := range decoders {
		if decoder == nil {
			continue
		}
		if value, ok := decoder(record); ok {
			return value
		}
	}
	return nil
}
