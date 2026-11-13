package graph

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/GizClaw/flowcraft/core/agent"
	"github.com/GizClaw/flowcraft/core/message"
	"github.com/goccy/go-json"
)

// errNoStore reports a read with nothing to read from.
var errNoStore = errors.New("graph: a store is required to read a timeline")

// Timeline is one task read back through this engine's record vocabulary.
//
// It is a view, not a second log: every fact stays an agent.EventRecord exactly
// as the store wrote it, and the only things added are the ones this engine is
// the authority on — where the run stood (projection) and how the facts group
// into frontiers.
type Timeline struct {
	Task        agent.Task
	Fingerprint agent.Fingerprint
	// Manifest is the version combination the run executed under, empty for a
	// task written before the engine recorded one.
	Manifest agent.Manifest
	// Round is the step ordinal the run stopped at, from the projection.
	Round int
	// Frontier is what the run was about to execute when it stopped.
	Frontier []string
	// ContextHash identifies the decision point. Empty when the run was recorded
	// with tracing off, which is a deployment choice rather than a gap.
	ContextHash string
	// Steps are the frontiers the task ran, in order. Records written before the
	// first marker — the input a durable run seeds — form the step at ordinal 0.
	Steps []TimelineStep
}

// TimelineStep is one frontier and the records it wrote.
type TimelineStep struct {
	// Ordinal is the 1-based step the marker names; 0 is the seeded input, which
	// no frontier ran.
	Ordinal int
	// Nodes are the frontier's node ids, in declaration order.
	Nodes []string
	// Replay reports that the step answered from records: no model was called.
	Replay bool
	// Records are this step's facts, ordered by sequence, undecoded: a kind this
	// build does not know is still a fact of the run, not a reason to hide it.
	Records []agent.EventRecord
}

// ReadOption configures one read.
type ReadOption func(*readConfig)

type readConfig struct {
	after int64
	limit int
}

// WithAfter reads only the records appended after seq, which is what a reader
// following a run incrementally asks for.
func WithAfter(seq int64) ReadOption {
	return func(c *readConfig) { c.after = seq }
}

// WithLimit stops after n records. Zero reads all of them.
func WithLimit(n int) ReadOption {
	return func(c *readConfig) { c.limit = n }
}

// Read loads one task and returns its timeline.
//
// The projection and the event log answer different questions and neither is a
// substitute for the other: the projection says where the run stands, the log
// says how it got there. A task the store does not hold reports
// [agent.ErrNoCheckpoint].
func Read(ctx context.Context, store agent.Store, taskID string, opts ...ReadOption) (*Timeline, error) {
	if store == nil {
		return nil, errNoStore
	}
	settings := readConfig{}
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
	timeline := &Timeline{
		Task:        checkpoint.Task,
		Fingerprint: checkpoint.Fingerprint,
	}
	if raw, ok := checkpoint.Sections[sectionGraph]; ok {
		var projection projection
		if json.Unmarshal(raw, &projection) == nil {
			timeline.Round = projection.Step
			timeline.Frontier = projection.Frontier
			timeline.ContextHash = projection.ContextHash
		}
	}
	if raw, ok := checkpoint.Sections[sectionManifest]; ok {
		var manifest agent.Manifest
		if json.Unmarshal(raw, &manifest) == nil {
			timeline.Manifest = manifest
		}
	}
	timeline.group(records)
	return timeline, nil
}

// ReadSession reads the newest task of one session, which is what "the last run"
// means to an operator.
func ReadSession(ctx context.Context, store agent.Store, sessionID string, opts ...ReadOption) (*Timeline, error) {
	if store == nil {
		return nil, errNoStore
	}
	checkpoint, err := store.LatestForSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return Read(ctx, store, checkpoint.Task.ID, opts...)
}

// group splits the log at its frontier markers.
//
// The engine writes the marker first, so every record after it belongs to that
// step. Records before the first marker are the seeded input of a durable run:
// ordinal 0, no frontier. A marker that does not decode still opens a step — the
// run did something there — with no nodes to name.
func (t *Timeline) group(records []agent.EventRecord) {
	for _, record := range records {
		if record.Kind == RecordFrontier {
			var marker frontierEvent
			_ = json.Unmarshal(record.Payload, &marker)
			t.Steps = append(t.Steps, TimelineStep{
				Ordinal: marker.Step,
				Nodes:   marker.Nodes,
				Replay:  marker.Replay,
			})
		} else if len(t.Steps) == 0 {
			t.Steps = append(t.Steps, TimelineStep{})
		}
		last := len(t.Steps) - 1
		t.Steps[last].Records = append(t.Steps[last].Records, record)
	}
}

// RenderOption configures [WriteText].
type RenderOption func(*renderConfig)

type renderConfig struct {
	width  int
	kinds  map[string]struct{}
	format func(agent.EventRecord) (string, bool)
}

// WithWidth clips one line to n characters. Zero uses the default.
func WithWidth(n int) RenderOption {
	return func(c *renderConfig) { c.width = n }
}

// WithKinds renders only these record kinds, which is what an operator asks for
// when following one thread ("only the model calls").
func WithKinds(kinds ...string) RenderOption {
	return func(c *renderConfig) {
		c.kinds = make(map[string]struct{}, len(kinds))
		for _, kind := range kinds {
			c.kinds[kind] = struct{}{}
		}
	}
}

// WithFormat renders records whose kind this engine does not own: another node
// kind's facts, or another engine's.
//
// A formatter returns false for a record it does not know, and the renderer falls
// back to the raw payload. The hook exists because the vocabulary is open by
// design: this package knows what it wrote, and cannot know what a node kind it
// was handed has.
func WithFormat(format func(agent.EventRecord) (string, bool)) RenderOption {
	return func(c *renderConfig) { c.format = format }
}

// WriteText renders a timeline for a person: one line per record under the step
// that produced it, with the time it happened.
//
// A record with no time — a row written before events carried one — prints "?"
// rather than a made-up clock: a wrong time is harder to notice than a missing
// one. A kind this build cannot decode prints its payload clipped rather than
// disappearing, because an operator looking at a broken run needs to see the
// record that does not fit.
func WriteText(w io.Writer, t *Timeline, opts ...RenderOption) error {
	settings := renderConfig{width: 120}
	for _, opt := range opts {
		if opt != nil {
			opt(&settings)
		}
	}
	if settings.width <= 0 {
		settings.width = 120
	}
	headline := fmt.Sprintf("task %s  %s  %d steps", t.Task.ID, t.Task.State, len(t.Steps))
	if t.Task.SessionID != "" && t.Task.SessionID != t.Task.ID {
		headline += "  session " + t.Task.SessionID
	}
	if id := t.Manifest.ID(); id != "" {
		headline += "  manifest " + clip(id, 20)
	}
	if t.ContextHash != "" {
		headline += "  context " + clip(t.ContextHash, 20)
	}
	if _, err := fmt.Fprintln(w, clip(headline, settings.width)); err != nil {
		return err
	}
	for _, step := range t.Steps {
		mode := "live"
		if step.Replay {
			mode = "replayed"
		}
		where := "seed"
		if step.Ordinal > 0 {
			where = fmt.Sprintf("step %d", step.Ordinal)
		}
		nodes := ""
		if len(step.Nodes) > 0 {
			nodes = "  " + strings.Join(step.Nodes, ", ")
		}
		heading := clip(fmt.Sprintf("%s%s  (%s)", where, nodes, mode), settings.width)
		if _, err := fmt.Fprintf(w, "\n%s\n", heading); err != nil {
			return err
		}
		for _, record := range step.Records {
			if settings.kinds != nil {
				if _, wanted := settings.kinds[record.Kind]; !wanted {
					continue
				}
			}
			// The marker is the step's heading, not a record of its own.
			if record.Kind == RecordFrontier {
				continue
			}
			line := fmt.Sprintf("  %s  %s", renderedAt(record), summarizeRecord(record, settings))
			if _, err := fmt.Fprintln(w, clip(line, settings.width)); err != nil {
				return err
			}
		}
	}
	return nil
}

// renderedAt renders the record's time, or "?" when the store could not date it.
func renderedAt(record agent.EventRecord) string {
	if record.At.IsZero() {
		return "?"
	}
	return record.At.Format("15:04:05.000")
}

// summarizeRecord renders one record as a line: the vocabulary of the kind when
// this build knows it, the raw bytes otherwise.
func summarizeRecord(record agent.EventRecord, settings renderConfig) string {
	width := settings.width
	switch record.Kind {
	case RecordLLMCall:
		var call agent.LLMCallRecord
		if json.Unmarshal(record.Payload, &call) == nil {
			return summarizeCall(call, width)
		}
	case RecordEffect:
		var effect agent.EffectRecord
		if json.Unmarshal(record.Payload, &effect) == nil {
			return fmt.Sprintf("%-12s %s %-7s %4dms  args=%s",
				"effect", effect.ToolName, effect.Status, effect.DurationMs, clip(effect.ArgsHash, 14))
		}
	case RecordState:
		var write stateEvent
		if json.Unmarshal(record.Payload, &write) == nil {
			return fmt.Sprintf("%-12s %-10s %s", "state", write.Key, summarizeValue(write.Value, width))
		}
	case eventBase:
		var base baseEvent
		if json.Unmarshal(record.Payload, &base) == nil {
			return fmt.Sprintf("%-12s step %d  blob %s", "base", base.Step, clip(base.Blob, 16))
		}
	case "message":
		var msg message.Message
		if json.Unmarshal(record.Payload, &msg) == nil {
			return fmt.Sprintf("%-12s %-9s %s", "message", msg.Role, clip(oneLine(msg.Content.Text()), width/2))
		}
	}
	if settings.format != nil {
		if line, ok := settings.format(record); ok {
			return line
		}
	}
	return fmt.Sprintf("%-12s %s", record.Kind, clip(oneLine(string(record.Payload)), width))
}

// summarizeCall renders one model call: what answered, what it cost, and the
// request's identity, which is what a replay looks the call up by.
func summarizeCall(call agent.LLMCallRecord, width int) string {
	modelName := call.Model.ID.Name
	if resolved := call.Metadata.Model.Name; resolved != "" && resolved != modelName {
		modelName += " → " + resolved
	}
	line := fmt.Sprintf("%-12s %-8s %s  in=%d out=%d  %dms",
		"llm.call", call.Model.ID.Provider, clip(modelName, 24),
		call.Usage.InputTokens, call.Usage.OutputTokens, call.LatencyMs)
	if call.ErrorKind != "" {
		line += "  " + call.ErrorKind
	}
	if call.ResponseRef != "" || len(call.Response) > 0 {
		line += "  response"
	}
	if call.Metadata.RequestID != "" {
		line += "  req=" + clip(call.Metadata.RequestID, 16)
	}
	return line
}

// summarizeValue renders one state value as the shape the key held, not the whole
// document: a run's transcript would otherwise fill the page.
func summarizeValue(value encodedValue, width int) string {
	switch value.Kind {
	case KindText:
		return clip(oneLine(value.Text), width/2)
	case KindBool:
		return fmt.Sprintf("%t", value.Bool)
	case KindMessages:
		if len(value.Messages) == 0 {
			return "messages(0)"
		}
		last := value.Messages[len(value.Messages)-1]
		return fmt.Sprintf("messages(%d)  %s: %s", len(value.Messages), last.Role,
			clip(oneLine(last.Content.Text()), width/2))
	case KindImage, KindAudio, KindVideo, KindFile:
		return fmt.Sprintf("%s(%s) %s", value.Kind, value.MediaType, clip(value.Blob, 16))
	}
	return clip(oneLine(string(value.JSON)), width/2)
}

// oneLine folds a multi-line payload so one fact stays one line.
func oneLine(text string) string {
	replaced := strings.NewReplacer("\n", " ", "\r", " ", "\t", " ")
	return strings.TrimSpace(replaced.Replace(text))
}

// clip shortens a string to n characters, marking that it did. It counts runes:
// a payload with a multi-byte character must not be cut in the middle of one.
func clip(text string, n int) string {
	if n <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= n {
		return text
	}
	if n == 1 {
		return string(runes[:1])
	}
	return string(runes[:n-1]) + "…"
}
