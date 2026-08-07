package sources

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/GizClaw/flowcraft/memory/storage"
	sdkmemory "github.com/GizClaw/flowcraft/sdk/memory"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestScopeCatalogConcurrentRegisterReopenAndSort(t *testing.T) {
	ctx := context.Background()
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
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
			writer, err := NewScopeCatalog(kvStore)
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
			writer, err := NewScopeCatalog(kvStore)
			if err == nil {
				err = writer.Register(ctx, scopes[0])
			}
			if err != nil {
				t.Errorf("concurrent duplicate Register: %v", err)
			}
		}()
	}
	wait.Wait()

	reopened, err := NewScopeCatalog(kvStore)
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

func TestScopeCatalogRejectsCorruptionAndUnknownSchema(t *testing.T) {
	ctx := context.Background()
	for _, data := range [][]byte{
		[]byte(`{"schema_version":`),
		[]byte(`{"schema_version":99,"scope":{"RuntimeID":"runtime","UserID":"user","AgentID":"agent"}}`),
	} {
		kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
		if err != nil {
			t.Fatal(err)
		}
		catalog, err := NewScopeCatalog(kvStore)
		if err != nil {
			t.Fatal(err)
		}
		scope := sdkmemory.Scope{RuntimeID: "runtime", UserID: "user", AgentID: "agent"}
		key, err := catalogKey(scope)
		if err != nil {
			t.Fatal(err)
		}
		if err := kvStore.Put(ctx, key, data); err != nil {
			t.Fatal(err)
		}
		if _, err := catalog.List(ctx); err == nil {
			t.Fatal("corrupt catalog entry was accepted")
		}
	}
}

func TestScopeCatalogValidationAndStoreRequirements(t *testing.T) {
	if _, err := NewScopeCatalog(nil); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := NewScopeCatalog(plainStore{}); err == nil {
		t.Fatal("store without immutable writes accepted")
	}
	kvStore, err := storage.NewWorkspaceKV(workspace.NewMemWorkspace())
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := NewScopeCatalog(kvStore)
	if err != nil {
		t.Fatal(err)
	}
	if err := catalog.Register(context.Background(), sdkmemory.Scope{}); err == nil {
		t.Fatal("invalid scope accepted")
	}
}

type plainStore struct{}

func (plainStore) Get(context.Context, string) ([]byte, error) {
	return nil, storage.ErrNotFound
}
func (plainStore) Put(context.Context, string, []byte) error { return nil }
func (plainStore) Delete(context.Context, string) error      { return nil }
func (plainStore) List(context.Context, string) ([]storage.Entry, error) {
	return []storage.Entry{}, nil
}
