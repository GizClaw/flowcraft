package message

import (
	"context"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Store persists canonical conversation messages.
type Store interface {
	Append(context.Context, AppendRequest) ([]Record, error)
	Commit(context.Context, AppendRequest) (Commit, error)
	Get(context.Context, sdkmemory.Scope, string, string) (Record, bool, error)
	List(context.Context, sdkmemory.Scope, string, ListOptions) ([]Record, error)
	Latest(context.Context, sdkmemory.Scope, string, LatestOptions) ([]Record, error)
	ListCommits(context.Context, sdkmemory.Scope, string, ListCommitOptions) ([]Commit, error)
	ListConversations(context.Context, sdkmemory.Scope) ([]string, error)
}
