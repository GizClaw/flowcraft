package document

import (
	"context"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// Store persists canonical external documents.
type Store interface {
	Put(context.Context, PutRequest) (Document, error)
	Get(context.Context, sdkmemory.Scope, string, string) (Document, bool, error)
	List(context.Context, sdkmemory.Scope, string, ListOptions) ([]Document, error)
	ListDatasets(context.Context, sdkmemory.Scope) ([]string, error)
	ListEvents(context.Context, sdkmemory.Scope, ListEventOptions) ([]Event, error)
	ListDocumentEvents(context.Context, sdkmemory.Scope, string, string, ListDocumentEventOptions) ([]Event, error)
	Delete(context.Context, sdkmemory.Scope, string, string) error
	DeleteDataset(context.Context, sdkmemory.Scope, string) error
}
