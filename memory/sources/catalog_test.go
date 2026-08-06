package sources

import (
	"context"
	"reflect"
	"sync"
	"testing"

	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestWorkspaceScopeCatalogConcurrentRegisterReopenAndSort(t *testing.T) {
	ctx := context.Background()
	ws := workspace.NewMemWorkspace()
	scopes := []sdkmemory.Scope{
		{RuntimeID: "runtime", UserID: "user", AgentID: "b"},
		{RuntimeID: "runtime", UserID: "user"},
		{RuntimeID: "runtime", UserID: "user", AgentID: "a"},
	}
	var wait sync.WaitGroup
	for _, scope := range scopes {
		scope := scope
		wait.Add(1)
		go func() {
			defer wait.Done()
			writer, err := NewWorkspaceScopeCatalog(ws)
			if err == nil {
				err = writer.Register(ctx, scope)
			}
			if err != nil {
				t.Errorf("Register(%v): %v", scope, err)
			}
		}()
	}
	for range 10 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			writer, err := NewWorkspaceScopeCatalog(ws)
			if err == nil {
				err = writer.Register(ctx, scopes[0])
			}
			if err != nil {
				t.Errorf("concurrent duplicate Register: %v", err)
			}
		}()
	}
	wait.Wait()

	reopened, err := NewWorkspaceScopeCatalog(ws)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []sdkmemory.Scope{scopes[1], scopes[2], scopes[0]}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("List = %#v, want %#v", got, want)
	}
}

func TestWorkspaceScopeCatalogRejectsCorruptionAndUnknownSchema(t *testing.T) {
	ctx := context.Background()
	for _, data := range [][]byte{
		[]byte(`{"schema_version":`),
		[]byte(`{"schema_version":99,"scope":{"RuntimeID":"runtime","UserID":"user","AgentID":"agent"}}`),
	} {
		ws := workspace.NewMemWorkspace()
		catalog, err := NewWorkspaceScopeCatalog(ws)
		if err != nil {
			t.Fatal(err)
		}
		scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
		ws.MustWrite(catalog.entryPath(scope), data)
		if _, err := catalog.List(ctx); err == nil {
			t.Fatal("corrupt catalog entry was accepted")
		}
	}
}
