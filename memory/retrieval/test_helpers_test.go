package retrieval

import (
	"testing"

	"github.com/GizClaw/flowcraft/memory/storage"
	factview "github.com/GizClaw/flowcraft/memory/views/fact"
	summaryview "github.com/GizClaw/flowcraft/memory/views/summary"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newFactStore(t *testing.T, ws workspace.Workspace, options ...factview.Option) *factview.FactStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := factview.NewFactStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func newSummaryStore(t *testing.T, ws workspace.Workspace, options ...summaryview.Option) *summaryview.SummaryStore {
	t.Helper()
	logStore, err := storage.NewWorkspaceLog(ws)
	if err != nil {
		t.Fatal(err)
	}
	kvStore, err := storage.NewWorkspaceKV(ws)
	if err != nil {
		t.Fatal(err)
	}
	store, err := summaryview.NewSummaryStore(logStore, kvStore, options...)
	if err != nil {
		t.Fatal(err)
	}
	return store
}
