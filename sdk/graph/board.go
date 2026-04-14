package graph

import (
	"github.com/GizClaw/flowcraft/sdk/workflow"
)

// Board is the graph execution blackboard (vars + message channels).
// Kanban card coordination lives in kanban.Board.
type Board = workflow.Board

// BoardSnapshot is serializable execution state (vars + channels only).
type BoardSnapshot = workflow.BoardSnapshot

// NewBoard creates an empty execution board.
func NewBoard() *Board {
	return workflow.NewBoard()
}

// RestoreBoard reconstructs a Board from a snapshot.
func RestoreBoard(snap *BoardSnapshot) *Board {
	return workflow.RestoreBoard(snap)
}

// GetTyped retrieves a typed value from the Board's vars.
func GetTyped[T any](b *Board, key string) (T, bool) {
	return workflow.GetTyped[T](b, key)
}
