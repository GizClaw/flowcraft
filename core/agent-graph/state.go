package graph

import (
	"fmt"
)

// Merge is how writes to one state key combine within a step.
type Merge string

const (
	// MergeSingle is the default: at most one node may write the key per step.
	// A second writer is an error rather than a silent winner, because neither
	// the graph document nor the reader can tell which one was meant.
	MergeSingle Merge = ""
	// MergeLast applies the writes in declaration order; the last one wins.
	// Choosing it is choosing a deterministic order, not "whatever finished first".
	MergeLast Merge = "last"
	// MergeAppend concatenates message sequences in declaration order. Each
	// writer supplies only what it added: the engine concatenates, so a node
	// that writes the cumulative session would duplicate the shared prefix.
	MergeAppend Merge = "append"
)

// Valid reports whether m is one of the declared merge rules.
func (m Merge) Valid() bool {
	switch m {
	case MergeSingle, MergeLast, MergeAppend:
		return true
	}
	return false
}

// StateKey declares one state slot.
type StateKey struct {
	Kind  Kind
	Merge Merge
}

// StateUpdate is one node's write to one state key.
type StateUpdate struct {
	Key   string
	Value Value
}

// stateStore holds the state between steps.
//
// Nodes never touch it: a step reads a snapshot taken before it started and its
// writes are applied afterwards, which is the same contract LangGraph gives a
// superstep and the reason a branch cannot observe a sibling's write.
type stateStore struct {
	values map[string]Value
}

func newStateStore(decl map[string]StateKey) *stateStore {
	store := &stateStore{values: make(map[string]Value, len(decl))}
	for key, spec := range decl {
		store.values[key] = zeroOf(spec.Kind)
	}
	return store
}

// zeroOf returns the value a key holds before anything writes it.
func zeroOf(kind Kind) Value {
	switch kind {
	case KindBool:
		return Bool(false)
	case KindMessages:
		return MessagesValue(EmptyMessages())
	case KindText:
		return Text("")
	case KindJSON:
		return JSON([]byte("null"))
	case KindImage, KindAudio, KindFile, KindVideo:
		return Value{kind: kind, mediaType: "application/octet-stream"}
	}
	return Value{}
}

// snapshot returns the wave-start view. One small map copy per step keeps reads
// consistent without a lock and without touching message bodies.
func (s *stateStore) snapshot() map[string]Value {
	out := make(map[string]Value, len(s.values))
	for key, value := range s.values {
		out[key] = value
	}
	return out
}

// apply merges one wave's writes in the order they were collected.
func (s *stateStore) apply(updates []StateUpdate, decl map[string]StateKey) error {
	written := make(map[string]bool, len(updates))
	for _, update := range updates {
		spec, ok := decl[update.Key]
		if !ok {
			return fmt.Errorf("graph: state key %q is not declared", update.Key)
		}
		if err := checkValue(spec.Kind, update.Value, "state "+update.Key); err != nil {
			return err
		}
		switch spec.Merge {
		case MergeSingle:
			if written[update.Key] {
				return fmt.Errorf(
					"graph: state key %q was written twice in one step; declare merge: last or merge: append",
					update.Key)
			}
			written[update.Key] = true
			s.values[update.Key] = update.Value
		case MergeLast:
			s.values[update.Key] = update.Value
		case MergeAppend:
			current, err := s.values[update.Key].Messages()
			if err != nil {
				return err
			}
			add, err := update.Value.Messages()
			if err != nil {
				return err
			}
			s.values[update.Key] = MessagesValue(current.Concat(add))
		}
	}
	return nil
}

// checkValue rejects a value whose kind does not match the declaration.
func checkValue(kind Kind, value Value, where string) error {
	if value.Kind() != kind {
		return fmt.Errorf("graph: %s is %q, want %s", where, value.Kind(), kind)
	}
	return nil
}
