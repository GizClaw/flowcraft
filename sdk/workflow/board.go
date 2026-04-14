// Package workflow defines the execution blackboard and high-level agent runtime types.
package workflow

import (
	"fmt"
	"maps"
	"reflect"
	"sync"

	"github.com/GizClaw/flowcraft/sdk/model"
)

// Cloneable may be implemented by values stored in Board vars
// to provide a type-safe deep copy instead of the JSON fallback.
type Cloneable interface {
	Clone() any
}

// MainChannel is the default message channel key (empty string).
const MainChannel = ""

// Board is the graph execution blackboard: typed message channels plus control vars.
// Card-based kanban coordination lives in kanban.Board, not here.
//
// Thread safety: public methods use a mutex (matches historical graph.Board behavior).
type Board struct {
	mu       sync.RWMutex
	channels map[string][]model.Message
	vars     map[string]any
}

// BoardSnapshot is a serializable representation of execution state (no kanban cards).
type BoardSnapshot struct {
	Vars     map[string]any             `json:"vars"`
	Channels map[string][]model.Message `json:"channels,omitempty"`
}

// NewBoard creates an empty Board with an initialized main channel.
func NewBoard() *Board {
	return &Board{
		channels: map[string][]model.Message{MainChannel: {}},
		vars:     make(map[string]any),
	}
}

// ---------- Vars ----------

// SetVar sets a board-level variable.
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

// GetVarString retrieves a board variable as a string, returning "" if missing or wrong type.
func (b *Board) GetVarString(key string) string {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if s, ok := b.vars[key].(string); ok {
		return s
	}
	return ""
}

// GetTyped retrieves a typed value from the Board's vars.
func GetTyped[T any](b *Board, key string) (T, bool) {
	raw, ok := b.GetVar(key)
	if !ok {
		var zero T
		return zero, false
	}
	v, ok := raw.(T)
	return v, ok
}

// Vars returns a shallow copy of all board-level variables.
func (b *Board) Vars() map[string]any {
	b.mu.RLock()
	defer b.mu.RUnlock()
	cp := make(map[string]any, len(b.vars))
	maps.Copy(cp, b.vars)
	return cp
}

// AppendSliceVar atomically appends a value to a slice stored in a board variable.
// It returns an error if the existing value is not a []any (instead of silently overwriting).
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
		return fmt.Errorf("board: var %q is %T, not []any", key, existing)
	}
	b.vars[key] = append(slice, value)
	return nil
}

// UpdateSliceVarItem finds and updates the first matching item in a slice variable.
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

// Channel returns a copy of messages for the given channel (empty slice if missing).
func (b *Board) Channel(name string) []model.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	msgs := b.channels[name]
	if len(msgs) == 0 {
		return nil
	}
	out := make([]model.Message, len(msgs))
	copy(out, msgs)
	return out
}

// SetChannel replaces the entire message list for a channel.
func (b *Board) SetChannel(name string, msgs []model.Message) {
	b.mu.Lock()
	if b.channels == nil {
		b.channels = map[string][]model.Message{}
	}
	cp := make([]model.Message, len(msgs))
	copy(cp, msgs)
	b.channels[name] = cp
	b.mu.Unlock()
}

// AppendChannelMessage appends a message to a channel.
func (b *Board) AppendChannelMessage(name string, msg model.Message) {
	b.mu.Lock()
	if b.channels == nil {
		b.channels = map[string][]model.Message{}
	}
	b.channels[name] = append(b.channels[name], msg)
	b.mu.Unlock()
}

// ChannelsCopy returns a deep copy of all channel message lists (for parallel merge).
func (b *Board) ChannelsCopy() map[string][]model.Message {
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(map[string][]model.Message, len(b.channels))
	for k, msgs := range b.channels {
		cp := make([]model.Message, len(msgs))
		copy(cp, msgs)
		out[k] = cp
	}
	return out
}

// ---------- Snapshot / Restore ----------

// Snapshot returns a serializable snapshot (vars + channels only).
func (b *Board) Snapshot() *BoardSnapshot {
	b.mu.RLock()
	defer b.mu.RUnlock()

	chCopy := make(map[string][]model.Message, len(b.channels))
	for k, msgs := range b.channels {
		cp := make([]model.Message, len(msgs))
		copy(cp, msgs)
		chCopy[k] = cp
	}
	return &BoardSnapshot{
		Vars:     deepCopyVars(b.vars),
		Channels: chCopy,
	}
}

// RestoreBoard reconstructs a Board from a snapshot.
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
			cp := make([]model.Message, len(msgs))
			copy(cp, msgs)
			b.channels[k] = cp
		}
	} else {
		b.channels[MainChannel] = []model.Message{}
	}
	migrateVarsMessages(b)
	return b
}

// RestoreFrom overwrites this board from a snapshot (executor retry rollback).
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
			cp := make([]model.Message, len(msgs))
			copy(cp, msgs)
			b.channels[k] = cp
		}
	}
	migrateVarsMessages(b)
}

// Deprecated: migrateVarsMessages moves legacy vars["messages"] into the main
// channel. Remove after v2 migration window.
func migrateVarsMessages(b *Board) {
	if msgs, ok := b.vars["messages"].([]model.Message); ok {
		if len(b.channels[MainChannel]) == 0 {
			cp := make([]model.Message, len(msgs))
			copy(cp, msgs)
			b.channels[MainChannel] = cp
		}
	}
	if len(b.channels) == 0 {
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
		out := make([]model.Message, len(val))
		copy(out, val)
		return out
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

// reflectDeepCopy recursively deep-copies a reflect.Value.
// Unsupported kinds (Chan, Func, UnsafePointer) are returned as-is.
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
