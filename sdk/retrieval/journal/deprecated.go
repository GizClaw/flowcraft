// Package journal is deprecated.
//
// Deprecated: use github.com/GizClaw/flowcraft/memory/retrieval/journal instead.
// This compatibility package will be removed in v0.5.0.
package journal

import target "github.com/GizClaw/flowcraft/memory/retrieval/journal"

type (
	ActorFn       = target.ActorFn
	Event         = target.Event
	Journal       = target.Journal
	MemoryJournal = target.MemoryJournal
	Op            = target.Op
	Option        = target.Option
)

const (
	OpDelete = target.OpDelete
	OpUpsert = target.OpUpsert
)

var (
	NewMemoryJournal = target.NewMemoryJournal
	WithActor        = target.WithActor
	Wrap             = target.Wrap
)
