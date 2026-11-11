package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/GizClaw/flowcraft/core/telemetry"
)

// The checkpoint rides the agent's envelope: the Task carries identity and
// lifecycle, the projection rides Sections, and the state itself is an
// append-only stream of two record kinds.
const (
	sectionGraph    = "graph"
	sectionManifest = "manifest"
	eventState      = "graph.state"
	eventBase       = "graph.base"
	eventLLMCall    = "llm.call"
	eventEffect     = "graph.effect"
)

// Record kinds a host needs when it prunes a trace: they are the records that
// survive state compaction, so a retention job names them explicitly.
const (
	// RecordLLMCall is one model call.
	RecordLLMCall = eventLLMCall
	// RecordEffect is one tool execution.
	RecordEffect = eventEffect
)

// projection is the small, per-step part of a checkpoint: where the run is, not
// what it holds.
type projection struct {
	Step     int      `json:"step"`
	Routed   int      `json:"routed"`
	Frontier []string `json:"frontier,omitempty"`
	Pending  []string `json:"pending,omitempty"`
	// StartedAt is the run's start, unix nanoseconds: a restored run keeps the
	// budget it already spent instead of getting a fresh timeout.
	StartedAt int64 `json:"started_at,omitempty"`
	// ContextHash identifies the decision point's projection. The prompt's own
	// identity is on each llm.call record.
	ContextHash string `json:"context_hash,omitempty"`
	// Base names the snapshot the projection stands on: restore reads that blob
	// and replays only the events after it.
	Base *baseRef `json:"base,omitempty"`
}

// baseRef is a confirmed snapshot: the blob holding the state and the sequence
// of the event that announced it.
type baseRef struct {
	Seq  int64  `json:"seq"`
	Blob string `json:"blob"`
}

// stateEvent records one write of one step, encoded so it stays small: a growing
// session contributes one message per write, not the whole transcript.
type stateEvent struct {
	Step  int          `json:"step"`
	Key   string       `json:"key"`
	Value encodedValue `json:"value"`
}

// baseEvent names a blob holding the whole state as of one step. A checkpoint
// writes a base when deltas would be missing (a failed save) or every N steps to
// bound replay.
type baseEvent struct {
	Step int    `json:"step"`
	Blob string `json:"blob"`
}

// encodedValue is a Value on the wire. Media crosses as a blob reference so the
// checkpoint never carries bytes.
type encodedValue struct {
	Kind      Kind              `json:"kind"`
	Text      string            `json:"text,omitempty"`
	Bool      bool              `json:"bool,omitempty"`
	JSON      json.RawMessage   `json:"json,omitempty"`
	Messages  []message.Message `json:"messages,omitempty"`
	MediaType string            `json:"media_type,omitempty"`
	Blob      string            `json:"blob,omitempty"`
}

// encodeValue renders one value, storing inline media bytes as a blob.
func encodeValue(ctx context.Context, blobs BlobStore, value Value) (encodedValue, error) {
	encoded := encodedValue{Kind: value.Kind()}
	switch value.Kind() {
	case KindText:
		text, err := value.Text()
		if err != nil {
			return encodedValue{}, err
		}
		encoded.Text = text
	case KindBool:
		flag, err := value.Bool()
		if err != nil {
			return encodedValue{}, err
		}
		encoded.Bool = flag
	case KindJSON:
		raw, err := value.JSON()
		if err != nil {
			return encodedValue{}, err
		}
		encoded.JSON = raw
	case KindMessages:
		messages, err := value.Messages()
		if err != nil {
			return encodedValue{}, err
		}
		encoded.Messages = messages.Slice()
	case KindImage, KindAudio, KindFile, KindVideo:
		media, err := value.Media()
		if err != nil {
			return encodedValue{}, err
		}
		encoded.MediaType = media.MediaType
		encoded.Blob = media.Blob
		if encoded.Blob == "" {
			if blobs == nil {
				return encodedValue{}, fmt.Errorf(
					"checkpoint: value %q carries inline bytes and no blob store is configured",
					value.Kind())
			}
			ref, err := blobs.PutBlob(ctx, media.Data)
			if err != nil {
				return encodedValue{}, fmt.Errorf("checkpoint: store media: %w", err)
			}
			encoded.Blob = ref
		}
	}
	return encoded, nil
}

// decodeValue reverses encodeValue. Media comes back as a reference.
func decodeValue(encoded encodedValue) (Value, error) {
	switch encoded.Kind {
	case KindText:
		return Text(encoded.Text), nil
	case KindBool:
		return Bool(encoded.Bool), nil
	case KindJSON:
		return JSON(encoded.JSON), nil
	case KindMessages:
		return MessagesValue(MessagesFromSlice(encoded.Messages)), nil
	case KindImage, KindAudio, KindFile, KindVideo:
		return mediaRef(encoded.Kind, encoded.MediaType, encoded.Blob), nil
	}
	return Value{}, fmt.Errorf("checkpoint: unknown value kind %q", encoded.Kind)
}

// RunOption configures a run beyond its input.
type RunOption func(*runOptions)

type runOptions struct {
	store       agent.Store
	taskID      string
	sessionID   string
	fingerprint agent.Fingerprint
	manifest    agent.Manifest
	trace       telemetry.TraceLevel
	replay      bool
}

// WithStore makes the run durable. The run id addresses the checkpoint; the
// session id lets a host find the newest run of a session.
func WithStore(store agent.Store, taskID string) RunOption {
	return func(o *runOptions) { o.store = store; o.taskID = taskID }
}

// WithSession names the session a run belongs to.
func WithSession(id string) RunOption {
	return func(o *runOptions) { o.sessionID = id }
}

// WithFingerprint sets the resume identity: a checkpoint taken under another
// deployment (or another graph) is refused.
func WithFingerprint(fingerprint agent.Fingerprint) RunOption {
	return func(o *runOptions) { o.fingerprint = fingerprint }
}

// WithManifest records the version combination the run executed under. The
// first save writes it into the checkpoint, where a badcase report can read it.
func WithManifest(manifest agent.Manifest) RunOption {
	return func(o *runOptions) { o.manifest = manifest }
}

// WithTrace sets how much of the run is traced.
func WithTrace(trace telemetry.TraceLevel) RunOption {
	return func(o *runOptions) { o.trace = trace }
}

// WithReplay answers model calls from the run's records instead of calling out.
func WithReplay(replay bool) RunOption {
	return func(o *runOptions) { o.replay = replay }
}

// persist writes the projection and this step's writes.
//
// A failed save is reported and does not fail the run: the next save writes a
// base instead of deltas, so the log never ends up with a hole. The cost is a
// checkpoint that lags by at most the unsaved steps.
func (r *Run) persist(ctx context.Context, updates []StateUpdate, records []agent.Record) error {
	if r.store == nil {
		return nil
	}
	// A node may checkpoint a side effect while the loop is between steps; one
	// save at a time keeps the base bookkeeping consistent.
	r.persistMu.Lock()
	defer r.persistMu.Unlock()
	events := make([]agent.EventRecord, 0, len(updates)+1)
	// A base is written when a delta would be missing, and every N steps so a
	// restart never replays more than the interval.
	due := r.g.baseEvery > 0 && r.step > 0 && r.step%r.g.baseEvery == 0
	var fresh *baseRef
	if r.needBase || due {
		if ref, ok := r.writeBase(ctx, &events); ok {
			fresh = &baseRef{Blob: ref}
			updates = nil
			r.needBase = false
		} else {
			// The base could not be built; keep the flag for the next attempt.
			r.needBase = true
		}
	}
	for _, record := range records {
		if record.Kind == "" || len(record.Payload) == 0 {
			continue
		}
		record = r.storedRecord(ctx, record)
		events = append(events, agent.EventRecord{Kind: record.Kind, Payload: record.Payload})
	}
	for _, update := range updates {
		encoded, err := encodeValue(ctx, r.store, update.Value)
		if err != nil {
			telemetry.WarnErr(ctx, "graph: encode state for checkpoint", err)
			r.needBase = true
			continue
		}
		payload, err := json.Marshal(stateEvent{Step: r.step, Key: update.Key, Value: encoded})
		if err != nil {
			r.needBase = true
			continue
		}
		events = append(events, agent.EventRecord{Kind: eventState, Payload: payload})
	}
	// The projection names the base the store already confirmed, or the one this
	// save is about to confirm: Save assigns the sequence, so a base can only be
	// named on the next save. A crash in between leaves the old base in place,
	// and replay from it treats the unconfirmed base as one more checkpoint.
	projected := r.namedBase
	if r.pendingBase != nil {
		projected = r.pendingBase
	}
	proj := projection{
		Step:      r.step,
		Routed:    r.routed,
		Frontier:  r.active,
		Pending:   r.pending,
		StartedAt: r.startedAt.UnixNano(),
		Base:      projected,
	}
	body, err := json.Marshal(proj)
	if err != nil {
		r.needBase = true
		return err
	}
	if r.trace != telemetry.TraceOff {
		sum := sha256.Sum256(body)
		proj.ContextHash = "sha256:" + hex.EncodeToString(sum[:])
		body, err = json.Marshal(proj)
		if err != nil {
			r.needBase = true
			return err
		}
	}
	sections := map[string]json.RawMessage{sectionGraph: body}
	writeManifest := !r.manifestWritten && r.manifest.ID() != ""
	if writeManifest {
		if raw, err := json.Marshal(r.manifest); err == nil {
			sections[sectionManifest] = raw
		} else {
			writeManifest = false
		}
	}
	checkpoint := agent.Checkpoint{
		Schema:      agent.CheckpointSchema,
		Fingerprint: r.fingerprint,
		Task: agent.Task{
			ID:        r.taskID,
			SessionID: r.sessionID,
			State:     r.state,
			Round:     r.step,
		},
		Sections: sections,
	}
	// The base the projection names is known before the save, so the events it
	// supersedes can go in the same transaction: one step, one commit.
	compactThrough := int64(0)
	if projected != nil && projected != r.namedBase {
		if projected.Seq > 1 && projected.Seq-1 > r.compactedThrough {
			compactThrough = projected.Seq - 1
		}
	}
	var options []agent.SaveOption
	if compactThrough > 0 {
		options = append(options, agent.TruncateThrough(compactThrough, eventState, eventBase))
	}
	first, _, err := r.store.Save(ctx, checkpoint, events, options...)
	if err != nil {
		telemetry.WarnErr(ctx, "graph: checkpoint write failed", err)
		r.needBase = true
		return err
	}
	if writeManifest {
		r.manifestWritten = true
	}
	for _, record := range records {
		r.indexRecord(record)
	}
	// The projection this save wrote is durable: promote its base. The log was
	// already compacted in the same transaction. Then a base written by this save
	// waits for the next projection.
	if projected != nil && projected != r.namedBase {
		r.namedBase = projected
		r.compactedThrough = compactThrough
	}
	if fresh != nil {
		fresh.Seq = first
		r.pendingBase = fresh
	}
	return nil
}

// storedRecord moves a model response into a blob, so the event log carries a
// reference rather than the answer's bytes.
func (r *Run) storedRecord(ctx context.Context, record agent.Record) agent.Record {
	if record.Kind != eventLLMCall || r.store == nil {
		return record
	}
	var call agent.LLMCallRecord
	if err := json.Unmarshal(record.Payload, &call); err != nil || len(call.Response) == 0 {
		return record
	}
	ref, err := r.store.PutBlob(ctx, call.Response)
	if err != nil {
		telemetry.WarnErr(ctx, "graph: store model response", err)
		return record
	}
	call.Response = nil
	call.ResponseRef = ref
	payload, err := json.Marshal(call)
	if err != nil {
		return record
	}
	return agent.Record{Kind: record.Kind, Payload: payload}
}

// indexRecord adds a recorded model call to the replay index.
func (r *Run) indexRecord(record agent.Record) {
	if record.Kind != eventLLMCall {
		return
	}
	var call agent.LLMCallRecord
	if err := json.Unmarshal(record.Payload, &call); err != nil || call.RequestHash == "" {
		return
	}
	if r.calls == nil {
		r.calls = make(map[string]agent.LLMCallRecord)
	}
	r.calls[call.RequestHash] = call
}

// writeBase snapshots the state and appends the event that names it.
func (r *Run) writeBase(ctx context.Context, events *[]agent.EventRecord) (string, bool) {
	body, err := r.encodeState(ctx)
	if err != nil {
		return "", false
	}
	ref, err := r.store.PutBlob(ctx, body)
	if err != nil {
		return "", false
	}
	payload, err := json.Marshal(baseEvent{Step: r.step, Blob: ref})
	if err != nil {
		telemetry.WarnErr(ctx, "graph: encode checkpoint base", err)
		return "", false
	}
	*events = append(*events, agent.EventRecord{Kind: eventBase, Payload: payload})
	return ref, true
}

// encodeState renders the whole state for a base snapshot.
func (r *Run) encodeState(ctx context.Context) ([]byte, error) {
	values := make(map[string]encodedValue, len(r.values.values))
	for key, value := range r.values.values {
		encoded, err := encodeValue(ctx, r.store, value)
		if err != nil {
			return nil, err
		}
		values[key] = encoded
	}
	return json.Marshal(values)
}

// restore rebuilds a run from its checkpoint. found is false when the store holds
// nothing for the id.
func (g *Graph) restore(ctx context.Context, opts runOptions) (*Run, bool, error) {
	if opts.store == nil {
		return nil, false, nil
	}
	checkpoint, err := opts.store.Load(ctx, opts.taskID)
	if errors.Is(err, agent.ErrNoCheckpoint) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if opts.fingerprint != "" && checkpoint.Fingerprint != opts.fingerprint {
		return nil, false, fmt.Errorf(
			"graph: checkpoint was taken under %s, this deployment is %s",
			checkpoint.Fingerprint, opts.fingerprint)
	}
	var proj projection
	if raw, ok := checkpoint.Sections[sectionGraph]; ok {
		if err := json.Unmarshal(raw, &proj); err != nil {
			return nil, false, fmt.Errorf("graph: decode projection: %w", err)
		}
	}
	values, calls, uncertain, err := g.restoreState(ctx, opts, proj.Base)
	if err != nil {
		return nil, false, err
	}
	if len(uncertain) > 0 {
		return nil, false, fmt.Errorf(
			"%w: %s", agent.ErrEffectUncertain, strings.Join(uncertain, ", "))
	}
	manifest := opts.manifest
	manifestWritten := false
	if raw, ok := checkpoint.Sections[sectionManifest]; ok {
		manifestWritten = true
		if manifest == (agent.Manifest{}) {
			_ = json.Unmarshal(raw, &manifest)
		}
	}
	run := &Run{
		g:               g,
		store:           opts.store,
		taskID:          opts.taskID,
		sessionID:       opts.sessionID,
		fingerprint:     opts.fingerprint,
		manifest:        manifest,
		manifestWritten: manifestWritten,
		trace:           opts.trace,
		calls:           calls,
		replay:          opts.replay,
		values:          values,
		active:          proj.Frontier,
		pending:         proj.Pending,
		step:            proj.Step,
		routed:          proj.Routed,
		state:           checkpoint.Task.State,
		namedBase:       proj.Base,
	}
	startedAt := time.Now()
	if proj.StartedAt > 0 {
		startedAt = time.Unix(0, proj.StartedAt)
	}
	run.startedAt = startedAt
	if g.timeout > 0 {
		// The timeout bounds the whole run, suspension included: a task that
		// waited longer than its budget is not resumed as if fresh.
		run.deadline = startedAt.Add(g.timeout)
	}
	return run, true, nil
}

// restoreState replays the event log from the projection's base: the blob gives
// the state as of that sequence, and only the events after it are read.
func (g *Graph) restoreState(ctx context.Context, opts runOptions, base *baseRef) (*stateStore, map[string]agent.LLMCallRecord, []string, error) {
	store := newStateStore(g.state)
	calls := make(map[string]agent.LLMCallRecord)
	started := make(map[string]agent.EffectRecord)
	var after int64
	if base != nil {
		if err := g.loadBase(ctx, opts, store, base.Blob); err != nil {
			return nil, nil, nil, err
		}
		after = base.Seq
	}
	events, err := opts.store.EventsAfter(ctx, opts.taskID, after)
	if err != nil {
		return nil, nil, nil, err
	}
	for i := 0; i < len(events); {
		event := events[i]
		switch event.Kind {
		case eventBase:
			var record baseEvent
			if err := json.Unmarshal(event.Payload, &record); err != nil {
				return nil, nil, nil, fmt.Errorf("graph: decode base: %w", err)
			}
			if err := g.loadBase(ctx, opts, store, record.Blob); err != nil {
				return nil, nil, nil, err
			}
			i++
		case eventLLMCall:
			var call agent.LLMCallRecord
			if err := json.Unmarshal(event.Payload, &call); err != nil {
				return nil, nil, nil, fmt.Errorf("graph: decode model call: %w", err)
			}
			if call.RequestHash != "" {
				calls[call.RequestHash] = call
			}
			i++
		case eventEffect:
			var effect agent.EffectRecord
			if err := json.Unmarshal(event.Payload, &effect); err != nil {
				return nil, nil, nil, fmt.Errorf("graph: decode effect: %w", err)
			}
			if effect.Status == agent.EffectStarted {
				started[effect.CallID] = effect
			} else {
				delete(started, effect.CallID)
			}
			i++
		case eventState:
			// One step is one merge: group the writes that share a step number,
			// apply them, then continue with the next group.
			var first stateEvent
			if err := json.Unmarshal(event.Payload, &first); err != nil {
				return nil, nil, nil, fmt.Errorf("graph: decode state event: %w", err)
			}
			updates := make([]StateUpdate, 0, 4)
			for {
				value, err := decodeValue(first.Value)
				if err != nil {
					return nil, nil, nil, err
				}
				updates = append(updates, StateUpdate{Key: first.Key, Value: value})
				i++
				if i >= len(events) || events[i].Kind != eventState {
					break
				}
				var next stateEvent
				if err := json.Unmarshal(events[i].Payload, &next); err != nil {
					return nil, nil, nil, fmt.Errorf("graph: decode state event: %w", err)
				}
				if next.Step != first.Step {
					break
				}
				first = next
			}
			if err := store.apply(updates, g.state); err != nil {
				return nil, nil, nil, err
			}
		default:
			i++
		}
	}
	uncertain := make([]string, 0, len(started))
	for callID := range started {
		uncertain = append(uncertain, callID)
	}
	sort.Strings(uncertain)
	return store, calls, uncertain, nil
}

// loadBase replaces the state with the one the blob holds.
func (g *Graph) loadBase(ctx context.Context, opts runOptions, target *stateStore, ref string) error {
	body, err := opts.store.GetBlob(ctx, ref)
	if err != nil {
		return fmt.Errorf("graph: read base: %w", err)
	}
	var encoded map[string]encodedValue
	if err := json.Unmarshal(body, &encoded); err != nil {
		return fmt.Errorf("graph: decode base state: %w", err)
	}
	clear(target.values)
	for key, value := range encoded {
		decoded, err := decodeValue(value)
		if err != nil {
			return err
		}
		target.values[key] = decoded
	}
	return nil
}
