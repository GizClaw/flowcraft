package document

import (
	"context"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Store publishes and reads complete document chunk builds.
type Store interface {
	ReplaceDocument(context.Context, ReplaceRequest) ([]Chunk, error)
	Get(context.Context, sdkmemory.Scope, string, string, string) (Chunk, bool, error)
	List(context.Context, sdkmemory.Scope, string, string, ListOptions) ([]Chunk, error)
}
