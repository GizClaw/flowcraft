package graph

import (
	"github.com/GizClaw/flowcraft/sdk/engine"
)

// Board is the graph execution blackboard (vars + message channels).
//
// Implementation lives in the engine package; graph aliases it so nodes,
// the executor and the validator share a single Board type with the rest
// of the runtime. Kanban card coordination lives in kanban.Board.
type Board = engine.Board

// BoardSnapshot is serialisable execution state (vars + channels only).
type BoardSnapshot = engine.BoardSnapshot

// MainChannel is the default message channel key (empty string). Re-exported
// from engine so nodes can reference graph.MainChannel without importing the
// engine package directly.
const MainChannel = engine.MainChannel

// Cloneable may be implemented by values stored in Board vars to provide a
// type-safe deep copy instead of the reflection fallback used by Snapshot
// and RestoreBoard.
type Cloneable = engine.Cloneable

// NewBoard creates an empty execution board.
func NewBoard() *Board {
	return engine.NewBoard()
}

// RestoreBoard reconstructs a Board from a snapshot.
func RestoreBoard(snap *BoardSnapshot) *Board {
	return engine.RestoreBoard(snap)
}

// GetTyped retrieves a typed value from the Board's vars.
func GetTyped[T any](b *Board, key string) (T, bool) {
	return engine.GetTyped[T](b, key)
}
