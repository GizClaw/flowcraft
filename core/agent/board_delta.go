package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"

	"github.com/GizClaw/flowcraft/core/message"
)

// BoardMark is a position in a board's write history. It is what turns a full
// copy into the write set that followed it, and it is deliberately cheap: one
// counter plus the length of every channel, taken under the board's read lock.
//
// A mark belongs to the board that produced it and to boards restored from a
// snapshot of that board. Taking a mark immediately after seeding a board is
// the contract that makes the result mean "the writes I am about to make, and
// nothing else" — `graph.ParallelConfig` seeds each branch and takes its mark
// that way.
//
// A mark becomes stale when the board is restored through [Board.RestoreFrom]:
// that is modelled as "every key was written", so a stale mark yields a
// conservative (larger) delta rather than a wrong, smaller one.
type BoardMark struct {
	seq      uint64
	channels map[string]int
}

// BoardDelta is the set of writes a board received after a [BoardMark].
//
// The values are borrowed from the board, not copied: a delta is valid only
// while the board it came from is not written again. That is what makes it
// cheap enough for hot paths, and it is safe because board values are
// immutable once written (see the [Board] contract). Callers that retain a
// delta — a checkpoint store encoding it, a parallel branch handing it to the
// merge — consume it without mutating the borrowed messages.
//
// A delta is complete in one direction only: it reports every write that
// happened through the [Board] API after the mark, including writes that
// restore state to what it already was. It cannot see a caller mutating a var
// value it read from the board; that is the immutable-value contract, not a
// gap this type can close.
type BoardDelta struct {
	vars     map[string]any
	removed  []string
	appends  map[string][]message.Message
	replaced map[string][]message.Message
}

// boardDeltaWire is the durable shape of a [BoardDelta]. Kept separate from
// the struct so the in-memory fields can stay borrowed and unexported.
type boardDeltaWire struct {
	Vars     map[string]any               `json:"vars,omitempty"`
	Removed  []string                     `json:"removed,omitempty"`
	Appends  map[string][]message.Message `json:"appends,omitempty"`
	Replaced map[string][]message.Message `json:"replaced,omitempty"`
}

// Mark records the board's current write position.
func (b *Board) Mark() BoardMark {
	if b == nil {
		return BoardMark{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	mark := BoardMark{seq: b.seq, channels: make(map[string]int, len(b.channels))}
	for name, msgs := range b.channels {
		mark.channels[name] = len(msgs)
	}
	return mark
}

// Since returns the writes this board received after mark.
//
// The returned delta is never nil; [BoardDelta.Empty] reports whether the
// board is unchanged. Values are borrowed — see [BoardDelta].
func (b *Board) Since(mark BoardMark) *BoardDelta {
	delta := &BoardDelta{}
	if b == nil {
		return delta
	}
	b.mu.RLock()
	defer b.mu.RUnlock()

	for key, seq := range b.varSeq {
		if seq <= mark.seq {
			continue
		}
		if value, ok := b.vars[key]; ok {
			if delta.vars == nil {
				delta.vars = make(map[string]any)
			}
			delta.vars[key] = value
			continue
		}
		delta.removed = append(delta.removed, key)
	}
	slices.Sort(delta.removed)

	for name, msgs := range b.channels {
		// A channel replaced or truncated since the mark is reported whole:
		// length comparison alone would miss a same-length replacement.
		if b.chanSeq[name] > mark.seq {
			if delta.replaced == nil {
				delta.replaced = make(map[string][]message.Message)
			}
			delta.replaced[name] = msgs
			continue
		}
		if base := mark.channels[name]; len(msgs) > base {
			if delta.appends == nil {
				delta.appends = make(map[string][]message.Message)
			}
			delta.appends[name] = msgs[base:]
		}
	}
	// A channel the board no longer has (a restore can drop one) is reported
	// as a replacement with no messages: a delta that cannot say "gone" would
	// leave stale history on the replaying side.
	for name := range mark.channels {
		if _, ok := b.channels[name]; ok {
			continue
		}
		if delta.replaced == nil {
			delta.replaced = make(map[string][]message.Message)
		}
		delta.replaced[name] = nil
	}
	return delta
}

// SinceMark returns [Board.Since] together with the mark that follows it, taken
// under one read lock. A writer that called Since and then Mark would leave a
// window in which a concurrent write is reported by neither, so the durable
// record would silently miss it; taking both at once closes that window.
func (b *Board) SinceMark(mark BoardMark) (*BoardDelta, BoardMark) {
	delta := b.Since(mark)
	return delta, b.Mark()
}

// noteVarWriteLocked records a var write at the current sequence. The caller
// holds the write lock.
func (b *Board) noteVarWriteLocked(key string) {
	b.seq++
	if b.varSeq == nil {
		b.varSeq = make(map[string]uint64)
	}
	b.varSeq[key] = b.seq
}

// noteChannelReplaceLocked records a channel write that is not a pure append
// (a wholesale set, or a pop). Length comparison cannot see those, so the
// channel is stamped and [Board.Since] reports it whole. The caller holds the
// write lock.
func (b *Board) noteChannelReplaceLocked(name string) {
	b.seq++
	if b.chanSeq == nil {
		b.chanSeq = make(map[string]uint64)
	}
	b.chanSeq[name] = b.seq
}

// noteRestoreLocked marks every key and channel the restore touched as
// written: the ones it installed and the ones it removed. Both matter, because
// a delta taken against an older mark must describe the whole transition —
// marking only what came back would leave a dropped key or channel looking
// untouched, and the replay would keep state the board no longer has.
//
// previous is the var map the restore replaced; its keys that are absent
// afterwards are the removals. The caller holds the write lock.
func (b *Board) noteRestoreLocked(previous map[string]any) {
	b.seq++
	if b.varSeq == nil {
		b.varSeq = make(map[string]uint64, len(b.vars))
	}
	for key := range b.vars {
		b.varSeq[key] = b.seq
	}
	for key := range previous {
		if _, ok := b.vars[key]; !ok {
			b.varSeq[key] = b.seq
		}
	}
	if b.chanSeq == nil {
		b.chanSeq = make(map[string]uint64, len(b.channels))
	}
	for name := range b.channels {
		b.chanSeq[name] = b.seq
	}
}

// SnapshotDelta returns the writes that turn base into next: a delta for
// consumers that hold states rather than live boards — a record a store handed
// back, and the state an engine wants persisted on top of it.
//
// Var values are compared, so an unchanged key costs a comparison and no copy.
// Channels follow the same append-only convention as the rest of the board: a
// channel that grew is reported as a suffix, anything else (shorter, or a
// prefix that no longer matches at the seam) as a replacement.
func SnapshotDelta(base, next *BoardSnapshot) *BoardDelta {
	delta := &BoardDelta{}
	if next == nil {
		return delta
	}
	var baseVars map[string]any
	var baseChannels map[string][]message.Message
	if base != nil {
		baseVars = base.Vars
		baseChannels = base.Channels
	}

	for key, value := range next.Vars {
		if prev, ok := baseVars[key]; ok && reflect.DeepEqual(prev, value) {
			continue
		}
		if delta.vars == nil {
			delta.vars = make(map[string]any)
		}
		delta.vars[key] = value
	}
	for key := range baseVars {
		if _, ok := next.Vars[key]; ok {
			continue
		}
		delta.removed = append(delta.removed, key)
	}
	slices.Sort(delta.removed)

	for name := range baseChannels {
		if _, ok := next.Channels[name]; ok {
			continue
		}
		if delta.replaced == nil {
			delta.replaced = make(map[string][]message.Message)
		}
		delta.replaced[name] = nil
	}
	for name, msgs := range next.Channels {
		baseMsgs := baseChannels[name]
		if len(msgs) > len(baseMsgs) && continuesChannel(baseMsgs, msgs) {
			if delta.appends == nil {
				delta.appends = make(map[string][]message.Message)
			}
			delta.appends[name] = msgs[len(baseMsgs):]
			continue
		}
		if len(msgs) == len(baseMsgs) && continuesChannel(baseMsgs, msgs) {
			continue
		}
		if delta.replaced == nil {
			delta.replaced = make(map[string][]message.Message)
		}
		delta.replaced[name] = msgs
	}
	return delta
}

// continuesChannel reports whether next still carries base as its prefix.
//
// It samples the ends rather than walking the history: a channel is
// append-only by convention, so a rewrite shows up either at the head (a
// preparer that drops the first message) or at the seam (the tail the two
// sides share). A rewrite that keeps both ends identical is not detected — that
// is the append-only convention, stated where the delta is applied rather than
// paid for on every comparison.
func continuesChannel(base, next []message.Message) bool {
	if len(base) == 0 {
		return true
	}
	if len(next) < len(base) {
		return false
	}
	if !reflect.DeepEqual(base[0], next[0]) {
		return false
	}
	return reflect.DeepEqual(base[len(base)-1], next[len(base)-1])
}

// Detach copies the memory the delta borrows, so it can outlive the board that
// produced it. A store that keeps a delta — a test double, an in-memory queue —
// calls it (or encodes instead); the delta a writer hands out is only valid
// while the board is not written again.
func (d *BoardDelta) Detach() {
	if d == nil {
		return
	}
	if d.vars != nil {
		d.vars = deepCopyVars(d.vars)
	}
	d.removed = slices.Clone(d.removed)
	for name, msgs := range d.appends {
		d.appends[name] = message.CloneMessages(msgs)
	}
	for name, msgs := range d.replaced {
		if msgs == nil {
			continue
		}
		d.replaced[name] = message.CloneMessages(msgs)
	}
}

// Empty reports whether the delta carries no write at all.
func (d *BoardDelta) Empty() bool {
	if d == nil {
		return true
	}
	return len(d.vars) == 0 && len(d.removed) == 0 &&
		len(d.appends) == 0 && len(d.replaced) == 0
}

// Vars returns the vars the delta sets, keyed by name. The map is the delta's
// own; the values are borrowed from the board.
func (d *BoardDelta) Vars() map[string]any {
	if d == nil {
		return nil
	}
	return d.vars
}

// Removed returns the var names the delta deletes, sorted.
func (d *BoardDelta) Removed() []string {
	if d == nil {
		return nil
	}
	return d.removed
}

// Appends returns the messages appended to each channel after the mark.
func (d *BoardDelta) Appends() map[string][]message.Message {
	if d == nil {
		return nil
	}
	return d.appends
}

// Replaced returns the channels the delta replaces outright, either because
// they were set wholesale or because they shrank.
func (d *BoardDelta) Replaced() map[string][]message.Message {
	if d == nil {
		return nil
	}
	return d.replaced
}

// ApplyTo writes the delta onto a board, cloning every value it carries so the
// receiving board owns its state (the delta keeps its borrowed references).
func (d *BoardDelta) ApplyTo(b *Board) {
	if d == nil || b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.vars == nil {
		b.vars = make(map[string]any)
	}
	if b.channels == nil {
		b.channels = map[string][]message.Message{MainChannel: {}}
	}
	b.applyDeltaLocked(d)
}

// ApplyToSnapshot writes the delta onto a snapshot (the replay direction a
// checkpoint store uses when it rebuilds a record from a base plus its
// deltas).
func (d *BoardDelta) ApplyToSnapshot(snap *BoardSnapshot) {
	if d == nil || snap == nil {
		return
	}
	if snap.Vars == nil {
		snap.Vars = make(map[string]any)
	}
	for _, key := range d.removed {
		delete(snap.Vars, key)
	}
	for key, value := range d.vars {
		snap.Vars[key] = deepCopyValue(value)
	}
	if len(d.replaced) > 0 && snap.Channels == nil {
		snap.Channels = make(map[string][]message.Message, len(d.replaced))
	}
	for name, msgs := range d.replaced {
		if msgs == nil {
			// A nil replacement means the channel is gone, not empty: a
			// restore can drop a channel outright.
			delete(snap.Channels, name)
			continue
		}
		snap.Channels[name] = message.CloneMessages(msgs)
	}
	for name, msgs := range d.appends {
		if len(msgs) == 0 {
			continue
		}
		if snap.Channels == nil {
			snap.Channels = make(map[string][]message.Message)
		}
		snap.Channels[name] = append(snap.Channels[name], message.CloneMessages(msgs)...)
	}
}

// applyDeltaLocked is the shared body of [BoardDelta.ApplyTo]; the caller
// holds the write lock.
func (b *Board) applyDeltaLocked(d *BoardDelta) {
	for _, key := range d.removed {
		delete(b.vars, key)
		b.noteVarWriteLocked(key)
	}
	for key, value := range d.vars {
		b.vars[key] = deepCopyValue(value)
		b.noteVarWriteLocked(key)
	}
	for name, msgs := range d.replaced {
		if msgs == nil {
			delete(b.channels, name)
		} else {
			b.channels[name] = message.CloneMessages(msgs)
		}
		b.noteChannelReplaceLocked(name)
	}
	for name, msgs := range d.appends {
		if len(msgs) == 0 {
			continue
		}
		out := b.channels[name]
		for _, msg := range msgs {
			out = append(out, msg.Clone())
		}
		b.channels[name] = out
	}
}

// MarshalJSON encodes the delta in a form [BoardDelta.UnmarshalJSON] restores.
func (d BoardDelta) MarshalJSON() ([]byte, error) {
	return json.Marshal(boardDeltaWire{
		Vars:     d.vars,
		Removed:  d.removed,
		Appends:  d.appends,
		Replaced: d.replaced,
	})
}

// UnmarshalJSON decodes a delta produced by [BoardDelta.MarshalJSON].
func (d *BoardDelta) UnmarshalJSON(data []byte) error {
	if d == nil {
		return fmt.Errorf("agent: decode board delta: nil receiver")
	}
	var wire boardDeltaWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return fmt.Errorf("agent: decode board delta: %w", err)
	}
	d.vars = wire.Vars
	d.removed = wire.Removed
	d.appends = wire.Appends
	d.replaced = wire.Replaced
	return nil
}
