package memory

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GizClaw/flowcraft/memory/sources"
	docsource "github.com/GizClaw/flowcraft/memory/sources/document"
	msgsource "github.com/GizClaw/flowcraft/memory/sources/message"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestSystemAdaptsThreeCapabilitiesAndIdempotency(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	messages, _ := msgsource.NewWorkspaceStore(ws)
	documents, _ := docsource.NewWorkspaceStore(ws)
	catalog, _ := sources.NewWorkspaceScopeCatalog(ws)
	provider := &captureProvider{result: sdkmemory.ContextResult{}}
	system, err := NewSystem(messages, documents, catalog, provider)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "alice"}
	turn := sdkmemory.Turn{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn-1",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "hello")},
		Metadata: sdkmemory.Metadata{"origin": "test"},
	}
	if err := system.CommitTurn(ctx, turn); err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}
	turn.Messages[0] = sdkmessage.NewTextMessage(sdkmessage.RoleUser, "changed")
	if err := system.CommitTurn(ctx, turn); err != nil {
		t.Fatal(err)
	}
	commits, err := messages.ListCommits(ctx, scope, "conversation", msgsource.ListCommitOptions{})
	if err != nil || len(commits) != 1 || commits[0].Version != 1 ||
		commits[0].Records[0].Message.Content.Text() != "hello" {
		t.Fatalf("commits = %#v, err=%v", commits, err)
	}

	document := sdkmemory.Document{
		Scope: scope, DatasetID: "dataset", DocumentID: "document", IdempotencyKey: "put-1",
		Content:    sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "content"}}},
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceExternal, ID: "source"}},
	}
	if err := system.PutDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	document.Content = sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: "changed"}}}
	if err := system.PutDocument(ctx, document); err != nil {
		t.Fatal(err)
	}
	stored, ok, err := documents.Get(ctx, scope, "dataset", "document")
	if err != nil || !ok || stored.Version != 1 || stored.Content.Text() != "content" {
		t.Fatalf("document = %#v, ok=%v err=%v", stored, ok, err)
	}

	request := sdkmemory.ContextRequest{
		Scope: scope, Query: "hello", DatasetIDs: []string{"dataset"},
		Metadata: sdkmemory.Metadata{"key": "value"},
	}
	if _, err := system.Context(ctx, request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(provider.request.DatasetIDs, request.DatasetIDs) ||
		!reflect.DeepEqual(provider.request.Metadata, request.Metadata) {
		t.Fatalf("context request = %#v", provider.request)
	}
	if system.MessageStore() != messages || system.DocumentStore() != documents {
		t.Fatal("read-only store accessors returned different stores")
	}
}

func TestSystemClassifiesValidationErrors(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	messages, _ := msgsource.NewWorkspaceStore(ws)
	documents, _ := docsource.NewWorkspaceStore(ws)
	catalog, _ := sources.NewWorkspaceScopeCatalog(ws)
	system, _ := NewSystem(messages, documents, catalog, &captureProvider{})
	if err := system.CommitTurn(context.Background(), sdkmemory.Turn{}); !sdkmemory.IsKind(err, sdkmemory.KindInvalidRequest) {
		t.Fatalf("CommitTurn error = %v", err)
	}
	if _, err := system.Context(nil, sdkmemory.ContextRequest{}); !sdkmemory.IsKind(err, sdkmemory.KindInvalidRequest) {
		t.Fatalf("Context error = %v", err)
	}
}

func TestSystemClassifiesStoreFailures(t *testing.T) {
	ws := workspace.NewMemWorkspace()
	messages, _ := msgsource.NewWorkspaceStore(ws)
	documents, _ := docsource.NewWorkspaceStore(ws)
	catalog, _ := sources.NewWorkspaceScopeCatalog(ws)
	cause := errors.New("storage unavailable")
	system, err := NewSystem(
		failingMessageStore{Store: messages, err: cause},
		failingDocumentStore{Store: documents, err: cause},
		catalog,
		&captureProvider{},
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := sdkmemory.Scope{RuntimeID: "runtime"}
	turnErr := system.CommitTurn(context.Background(), sdkmemory.Turn{
		Scope: scope, ConversationID: "conversation", IdempotencyKey: "turn",
		Messages: []sdkmessage.Message{sdkmessage.NewTextMessage(sdkmessage.RoleUser, "hello")},
	})
	if !sdkmemory.IsKind(turnErr, sdkmemory.KindProviderFailure) || !errors.Is(turnErr, cause) {
		t.Fatalf("CommitTurn error = %v", turnErr)
	}
	documentErr := system.PutDocument(context.Background(), sdkmemory.Document{
		Scope: scope, DatasetID: "dataset", DocumentID: "document", IdempotencyKey: "put",
		Content:    sdkmessage.NewTextMessage(sdkmessage.RoleUser, "content").Content,
		Provenance: []sdkmemory.SourceRef{{Kind: sdkmemory.SourceExternal, ID: "source"}},
	})
	if !sdkmemory.IsKind(documentErr, sdkmemory.KindProviderFailure) || !errors.Is(documentErr, cause) {
		t.Fatalf("PutDocument error = %v", documentErr)
	}
	scopes, err := catalog.List(context.Background())
	if err != nil || !reflect.DeepEqual(scopes, []sdkmemory.Scope{scope}) {
		t.Fatalf("catalog after source failures = %#v, %v", scopes, err)
	}
	conversations, _ := messages.ListConversations(context.Background(), scope)
	datasets, _ := documents.ListDatasets(context.Background(), scope)
	if len(conversations) != 0 || len(datasets) != 0 {
		t.Fatalf("failed source writes persisted data: conversations=%v datasets=%v", conversations, datasets)
	}
}

type failingMessageStore struct {
	msgsource.Store
	err error
}

func (store failingMessageStore) Commit(context.Context, msgsource.AppendRequest) (msgsource.Commit, error) {
	return msgsource.Commit{}, store.err
}

type failingDocumentStore struct {
	docsource.Store
	err error
}

func (store failingDocumentStore) Put(context.Context, docsource.PutRequest) (docsource.Document, error) {
	return docsource.Document{}, store.err
}

type captureProvider struct {
	request sdkmemory.ContextRequest
	result  sdkmemory.ContextResult
	err     error
}

func (provider *captureProvider) Context(_ context.Context, request sdkmemory.ContextRequest) (sdkmemory.ContextResult, error) {
	provider.request = request
	return provider.result, provider.err
}
