package fact

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	sdkmessage "github.com/GizClaw/flowcraft/sdk/message"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

var (
	factScope  = sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
	factSource = sdkmemory.SourceRef{Kind: sdkmemory.SourceMessage, ID: "message-1", Revision: "1"}
)

func TestWorkspaceStoreAddRetryConflictListAndClone(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	store := newFactStore(t, workspace.NewMemWorkspace(), WithClock(func() time.Time { return now }))
	request := factRequest("b", "second")
	got, err := store.Add(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := store.Add(ctx, request)
	if err != nil || !reflect.DeepEqual(got, retry) {
		t.Fatalf("retry = %#v, %v", retry, err)
	}
	conflict := request
	conflict.Content = textContent("different")
	if _, err := store.Add(ctx, conflict); !errdefs.IsConflict(err) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := store.Add(ctx, factRequest("a", "first")); err != nil {
		t.Fatal(err)
	}
	page, err := store.List(ctx, factScope, "conversation", ListOptions{Limit: 1})
	if err != nil || len(page) != 1 || page[0].ID != "a" {
		t.Fatalf("page = %#v, %v", page, err)
	}
	rest, err := store.List(ctx, factScope, "conversation", ListOptions{
		AfterCreatedAt: page[0].CreatedAt, AfterID: page[0].ID,
	})
	if err != nil || len(rest) != 1 || rest[0].ID != "b" {
		t.Fatalf("rest = %#v, %v", rest, err)
	}
	request.Metadata["key"] = "mutated"
	request.Provenance[0].ID = "mutated"
	got.Content.Parts[0] = sdkmessage.TextPart{Text: "mutated"}
	got.Metadata["key"] = "mutated"
	again, ok, err := store.Get(ctx, factScope, "conversation", "b")
	if err != nil || !ok || again.Content.Text() != "second" ||
		again.Provenance[0].ID != "message-1" || again.Metadata["key"] != "value" {
		t.Fatalf("stored fact aliased: %#v, %v, %v", again, ok, err)
	}
}

func TestWorkspaceStoreIsolationReopenTraversalAndConcurrency(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	store := newFactStore(t, ws)
	const count = 30
	var wait sync.WaitGroup
	errs := make(chan error, count)
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			_, err := store.Add(ctx, factRequest(fmt.Sprint(index), fmt.Sprint(index)))
			errs <- err
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	reopened := newFactStore(t, ws)
	listed, err := reopened.List(ctx, factScope, "conversation", ListOptions{})
	if err != nil || len(listed) != count {
		t.Fatalf("reopened list = %d, %v", len(listed), err)
	}
	otherScope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "other"}
	if _, err := reopened.Add(ctx, AddRequest{
		ID: "same", Scope: otherScope, ConversationID: "conversation",
		Content: textContent("other"), Provenance: []sdkmemory.SourceRef{factSource},
	}); err != nil {
		t.Fatal(err)
	}
	other, _ := reopened.List(ctx, otherScope, "conversation", ListOptions{})
	if len(other) != 1 || other[0].Content.Text() != "other" {
		t.Fatalf("partition leak: %#v", other)
	}
	malicious := sdkmemory.Scope{RuntimeID: "../runtime", UserID: "/../../user"}
	if _, err := reopened.Add(ctx, AddRequest{
		ID: "../fact", Scope: malicious, ConversationID: "../../conversation",
		Content: textContent("safe"), Provenance: []sdkmemory.SourceRef{factSource},
	}); err != nil {
		t.Fatal(err)
	}
	target := reopened.factPath(malicious, "../../conversation", "../fact")
	if strings.Contains(target, "..") || strings.HasPrefix(target, "/") {
		t.Fatalf("unsafe path %q", target)
	}
}

func TestWorkspaceStoreRejectsCorruptionAndUnknownSchema(t *testing.T) {
	ctx := context.Background()
	for _, data := range [][]byte{
		[]byte(`{"schema_version":`),
		[]byte(`{"schema_version":99}`),
	} {
		ws := workspace.NewMemWorkspace()
		store := newFactStore(t, ws)
		ws.MustWrite(store.factPath(factScope, "conversation", "fact"), data)
		if _, err := store.List(ctx, factScope, "conversation", ListOptions{}); err == nil {
			t.Fatal("corrupt data accepted")
		}
	}
	ws := workspace.NewMemWorkspace()
	store := newFactStore(t, ws)
	if _, err := store.Add(ctx, factRequest("fact", "text")); err != nil {
		t.Fatal(err)
	}
	data, _ := ws.Read(ctx, store.factPath(factScope, "conversation", "fact"))
	var raw map[string]any
	_ = json.Unmarshal(data, &raw)
	raw["fact_id"] = "wrong"
	data, _ = json.Marshal(raw)
	ws.MustWrite(store.factPath(factScope, "conversation", "fact"), data)
	if _, _, err := store.Get(ctx, factScope, "conversation", "fact"); err == nil {
		t.Fatal("address corruption accepted")
	}
}

func factRequest(id, text string) AddRequest {
	return AddRequest{
		ID: id, Scope: factScope, ConversationID: "conversation",
		Content: textContent(text), Provenance: []sdkmemory.SourceRef{factSource},
		Metadata: sdkmemory.Metadata{"key": "value"},
	}
}

func textContent(text string) sdkmessage.Content {
	return sdkmessage.Content{Parts: []sdkmessage.Part{sdkmessage.TextPart{Text: text}}}
}

func newFactStore(t *testing.T, ws workspace.Workspace, options ...Option) *WorkspaceStore {
	t.Helper()
	store, err := NewWorkspaceStore(ws, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
