package engine

import (
	"fmt"
	"maps"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// MainChannel is the default message channel key (empty string).
//
// Channels are an engine-level primitive: they let nodes/steps share
// ordered message sequences without going through Vars. Convention-level
// keys for "the chat transcript", "the answer", etc. belong to the
// agent layer; this package only provides the channel mechanism.
const MainChannel = ""

// Cloneable may be implemented by values stored in Board vars to
// provide a type-safe deep copy instead of the reflection fallback used
// by [Board.Snapshot] and [RestoreBoard].
type Cloneable interface {
	Clone() any
}

// Board is the engine execution blackboard: typed message channels plus
// untyped control vars, both protected by the same mutex.
//
// Thread safety: every public method takes a mutex. Concurrent reads use
// RLock; mutations use Lock. The contract matches the original
// graph.Board it replaces — callers that previously held graph.Board
// across goroutines do not need to add any new locking.
//
// Board is intentionally ignorant of agent concepts. It does not know
// what "messages", "answer" or "run id" mean; those names are
// established by callers. Per-execution metadata (ID, Attributes,
// Deps) belongs on [Run], not here.
type Board struct {
	mu       sync.RWMutex
	channels map[string][]model.Message
	vars     map[string]any
}

// BoardSnapshot is a serialisable representation of board state, used
// for resume / checkpoint flows. It carries no live mutex and is safe
// to JSON-encode.
type BoardSnapshot struct {
	Vars     map[string]any             `json:"vars"`
	Channels map[string][]model.Message `json:"channels,omitempty"`
}

// NewBoard creates an empty Board with an initialised main channel so
// callers can [Board.AppendChannelMessage] without a nil-check.
func NewBoard() *Board {
	return &Board{
		channels: map[string][]model.Message{MainChannel: {}},
		vars:     make(map[string]any),
	}
}

// ---------- Vars ----------

// SetVar sets a board-level variable. Concurrent-safe.
func (b *Board) SetVar(key string, value any) {
	b.mu.Lock()
	b.vars[key] = value
	b.mu.Unlock()
}

// GetVar retrieves a board-level variable.
func (b *Board) GetVar(key string) (any, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	v, ok := b.vars[key]
	return v, ok
}

// GetVarString retrieves a board variable as a string. It returns ""
// when the key is missing or the stored value is not a string.
func (b *Board) GetVarString(key string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if s, ok := b.vars[key].(string); ok {
		return s
	}
	return ""
}

// GetTyped retrieves a typed value from the Board's vars. It is a
// generic helper rather than a method because Go does not allow type
// parameters on methods.
func GetTyped[T any](b *Board, key string) (T, bool) {
	raw, ok := b.GetVar(key)
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := raw.(T)
	return v, ok
}

// Vars returns a shallow copy of all board-level variables. Mutations
// to the returned map do not affect the Board.
func (b *Board) Vars() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cp := make(map[string]any, len(b.vars))
	maps.Copy(cp, b.vars)
	return cp
}

// AppendSliceVar atomically appends a value to a slice stored in a
// board variable. It returns an error if the existing value is not a
// []any (instead of silently overwriting), so callers cannot lose data
// by typo'ing a key already used for a non-slice value.
func (b *Board) AppendSliceVar(key string, value any) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	existing, ok := b.vars[key]
	if !ok {
		b.vars[key] = []any{value}
		return nil
	}
	slice, ok := existing.([]any)
	if !ok {
		return fmt.Errorf("engine.Board: var %q is %T, not []any", key, existing)
	}
	b.vars[key] = append(slice, value)
	return nil
}

// UpdateSliceVarItem finds and updates the first matching item in a
// slice variable. Missing keys and non-slice values are silently
// ignored — the typical use is "update the entry I just appended", and
// the caller has already verified the slice exists.
func (b *Board) UpdateSliceVarItem(key string, match func(any) bool, update func(any) any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	existing, ok := b.vars[key]
	if !ok {
		return
	}
	slice, ok := existing.([]any)
	if !ok {
		return
	}
	for i, item := range slice {
		if match(item) {
			slice[i] = update(item)
			return
		}
	}
}

// ---------- Channels ----------

// Channel returns a copy of messages for the given channel. An empty
// or missing channel returns a nil slice (not a zero-length slice) so
// callers can use len() == 0 uniformly.
func (b *Board) Channel(name string) []model.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs := b.channels[name]
	if len(msgs) == 0 {
		return nil
	}
	return model.CloneMessages(msgs)
}

// SetChannel replaces the entire message list for a channel. The input
// slice is copied; later mutations by the caller do not affect the
// Board.
func (b *Board) SetChannel(name string, msgs []model.Message) {
	b.mu.Lock()
	if b.channels == nil {
		b.channels = map[string][]model.Message{}
	}
	b.channels[name] = model.CloneMessages(msgs)
	b.mu.Unlock()
}

// AppendChannelMessage appends a message to a channel, creating the
// channel on demand.
func (b *Board) AppendChannelMessage(name string, msg model.Message) {
	b.mu.Lock()
	if b.channels == nil {
		b.channels = map[string][]model.Message{}
	}
	b.channels[name] = append(b.channels[name], msg.Clone())
	b.mu.Unlock()
}

// ChannelsCopy returns a deep copy of all channel message lists. Used
// by parallel branch execution to give each branch an independent
// view that can later be merged.
func (b *Board) ChannelsCopy() map[string][]model.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string][]model.Message, len(b.channels))
	for k, msgs := range b.channels {
		out[k] = model.CloneMessages(msgs)
	}
	return out
}

// ---------- Snapshot / Restore ----------

// Snapshot returns a serialisable copy of the current state. Vars are
// deep-copied so the snapshot is safe to retain after further Board
// mutations; values implementing [Cloneable] are duplicated through
// their Clone method, otherwise reflection is used.
func (b *Board) Snapshot() *BoardSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	chCopy := make(map[string][]model.Message, len(b.channels))
	for k, msgs := range b.channels {
		chCopy[k] = model.CloneMessages(msgs)
	}
	return &BoardSnapshot{
		Vars:     deepCopyVars(b.vars),
		Channels: chCopy,
	}
}

// RestoreBoard reconstructs a Board from a snapshot. Passing nil
// returns a fresh empty board so resume code can use this
// unconditionally.
func RestoreBoard(snap *BoardSnapshot) *Board {
	if snap == nil {
		return NewBoard()
	}
	b := &Board{
		channels: make(map[string][]model.Message),
		vars:     deepCopyVars(snap.Vars),
	}
	if len(snap.Channels) > 0 {
		for k, msgs := range snap.Channels {
			b.channels[k] = model.CloneMessages(msgs)
		}
	} else {
		b.channels[MainChannel] = []model.Message{}
	}
	return b
}

// RestoreFrom overwrites this board from a snapshot. Used by retry /
// rollback paths inside an executor: the executor takes a snapshot
// before a risky step, then restores the same Board (preserving its
// identity for nodes that captured a pointer to it) on failure.
func (b *Board) RestoreFrom(snap *BoardSnapshot) {
	if snap == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.vars = deepCopyVars(snap.Vars)
	b.channels = make(map[string][]model.Message)
	if len(snap.Channels) > 0 {
		for k, msgs := range snap.Channels {
			b.channels[k] = model.CloneMessages(msgs)
		}
	} else {
		// Mirror RestoreBoard / NewBoard: every Board must expose
		// MainChannel even when the snapshot didn't carry channels.
		b.channels[MainChannel] = []model.Message{}
	}
}

// ---------- internal ----------

func deepCopyVars(src map[string]any) map[string]any {
	if src == nil {
		return make(map[string]any)
	}
	cp := make(map[string]any, len(src))
	for k, v := range src {
		cp[k] = deepCopyValue(v)
	}
	return cp
}

func deepCopyValue(v any) any {
	if v == nil {
		return nil
	}
	switch val := v.(type) {
	case string, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64,
		float32, float64, bool:
		return v
	case []model.Message:
		return model.CloneMessages(val)
	case []any:
		out := make([]any, len(val))
		for i, item := range val {
			out[i] = deepCopyValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(val))
		for k, item := range val {
			out[k] = deepCopyValue(item)
		}
		return out
	case Cloneable:
		return val.Clone()
	default:
		return reflectDeepCopy(reflect.ValueOf(v)).Interface()
	}
}

// reflectDeepCopy recursively deep-copies a reflect.Value. Unsupported
// kinds (Chan, Func, UnsafePointer) fall through and are returned as
// the original value — those are not meaningful to clone for snapshot.
func reflectDeepCopy(v reflect.Value) reflect.Value {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		cp := reflect.New(v.Type().Elem())
		cp.Elem().Set(reflectDeepCopy(v.Elem()))
		return cp
	case reflect.Slice:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		cp := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := range v.Len() {
			cp.Index(i).Set(reflectDeepCopy(v.Index(i)))
		}
		return cp
	case reflect.Map:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		cp := reflect.MakeMapWithSize(v.Type(), v.Len())
		iter := v.MapRange()
		for iter.Next() {
			cp.SetMapIndex(reflectDeepCopy(iter.Key()), reflectDeepCopy(iter.Value()))
		}
		return cp
	case reflect.Struct:
		cp := reflect.New(v.Type()).Elem()
		cp.Set(v)
		for i := range v.NumField() {
			f := cp.Field(i)
			if f.CanSet() {
				f.Set(reflectDeepCopy(v.Field(i)))
			}
		}
		return cp
	case reflect.Array:
		cp := reflect.New(v.Type()).Elem()
		for i := range v.Len() {
			cp.Index(i).Set(reflectDeepCopy(v.Index(i)))
		}
		return cp
	case reflect.Interface:
		if v.IsNil() {
			return reflect.Zero(v.Type())
		}
		inner := reflectDeepCopy(v.Elem())
		cp := reflect.New(v.Type()).Elem()
		cp.Set(inner)
		return cp
	default:
		return v
	}
}
