// Package memory wires the public SDK memory capabilities to canonical stores
// and a retrieval provider. It deliberately does not start background work.
package memory

import (
	"context"
	"errors"

	"github.com/GizClaw/flowcraft/memory/sources"
	docsource "github.com/GizClaw/flowcraft/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/memory/sources/message"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
)

// System implements the three SDK memory capabilities.
type System struct {
	messages  *msgsource.MessageStore
	documents *docsource.DocumentStore
	catalog   *sources.ScopeCatalog
	context   sdkmemory.ContextProvider
}

var (
	_ sdkmemory.ContextProvider = (*System)(nil)
	_ sdkmemory.TurnSink        = (*System)(nil)
	_ sdkmemory.DocumentSink    = (*System)(nil)
)

// NewSystem constructs a capability adapter without starting a worker.
func NewSystem(messages *msgsource.MessageStore, documents *docsource.DocumentStore, catalog *sources.ScopeCatalog, provider sdkmemory.ContextProvider) (*System, error) {
	if messages == nil {
		return nil, errors.New("memory system: message store is required")
	}
	if documents == nil {
		return nil, errors.New("memory system: document store is required")
	}
	if catalog == nil {
		return nil, errors.New("memory system: scope catalog is required")
	}
	if provider == nil {
		return nil, errors.New("memory system: context provider is required")
	}
	return &System{messages: messages, documents: documents, catalog: catalog, context: provider}, nil
}

// MessageStore returns the canonical message store.
func (system *System) MessageStore() *msgsource.MessageStore {
	if system == nil {
		return nil
	}
	return system.messages
}

// DocumentStore returns the canonical document store.
func (system *System) DocumentStore() *docsource.DocumentStore {
	if system == nil {
		return nil
	}
	return system.documents
}

func (system *System) Context(ctx context.Context, request sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	if system == nil || system.context == nil {
		return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindNotConfigured, "context", errors.New("memory system is incomplete"))
	}
	if ctx == nil {
		return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindInvalidRequest, "context", errors.New("context is required"))
	}
	if err := request.Validate(); err != nil {
		return sdkmemory.ContextResult{}, err
	}
	request.DatasetIDs = append([]string(nil), request.DatasetIDs...)
	request.Metadata = request.Metadata.Clone()
	result, err := system.context.Context(ctx, request)
	if err != nil {
		return sdkmemory.ContextResult{}, classify(err, "context", sdkmemory.KindProviderFailure)
	}
	if err := result.Validate(); err != nil {
		return sdkmemory.ContextResult{}, sdkmemory.NewError(sdkmemory.KindInternal, "context", err)
	}
	return result.Clone(), nil
}

func (system *System) CommitTurn(ctx context.Context, turn sdkmemory.Turn) error {
	if system == nil || system.messages == nil {
		return sdkmemory.NewError(sdkmemory.KindNotConfigured, "turn", errors.New("memory system is incomplete"))
	}
	if ctx == nil {
		return sdkmemory.NewError(sdkmemory.KindInvalidRequest, "turn", errors.New("context is required"))
	}
	if err := turn.Validate(); err != nil {
		return err
	}
	turn = turn.Clone()
	if err := system.catalog.Register(ctx, turn.Scope); err != nil {
		return classify(err, "turn", sdkmemory.KindProviderFailure)
	}
	_, err := system.messages.Commit(ctx, msgsource.AppendRequest{
		Scope: turn.Scope, ConversationID: turn.ConversationID,
		IdempotencyKey: turn.IdempotencyKey, Messages: turn.Messages, Metadata: turn.Metadata,
	})
	return classify(err, "turn", sdkmemory.KindProviderFailure)
}

func (system *System) PutDocument(ctx context.Context, document sdkmemory.Document) error {
	if system == nil || system.documents == nil {
		return sdkmemory.NewError(sdkmemory.KindNotConfigured, "document", errors.New("memory system is incomplete"))
	}
	if ctx == nil {
		return sdkmemory.NewError(sdkmemory.KindInvalidRequest, "document", errors.New("context is required"))
	}
	if err := document.Validate(); err != nil {
		return err
	}
	document = document.Clone()
	if err := system.catalog.Register(ctx, document.Scope); err != nil {
		return classify(err, "document", sdkmemory.KindProviderFailure)
	}
	_, err := system.documents.Put(ctx, docsource.PutRequest{
		Scope: document.Scope, DatasetID: document.DatasetID, DocumentID: document.DocumentID,
		IdempotencyKey: document.IdempotencyKey, Content: document.Content,
		Provenance: document.Provenance, Metadata: document.Metadata,
	})
	return classify(err, "document", sdkmemory.KindProviderFailure)
}

func classify(err error, capability string, fallback sdkmemory.ErrorKind) error {
	if err == nil {
		return nil
	}
	if sdkmemory.AsError(err) != nil {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return sdkmemory.NewError(sdkmemory.KindOperationInterrupted, capability, err)
	}
	return sdkmemory.NewError(fallback, capability, err)
}
