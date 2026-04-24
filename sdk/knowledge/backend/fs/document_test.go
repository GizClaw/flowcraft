package fs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/sdk/errdefs"
	"github.com/GizClaw/flowcraft/sdk/knowledge"
	"github.com/GizClaw/flowcraft/sdk/workspace"
)

func newRepo(t *testing.T) (*FSDocumentRepo, workspace.Workspace) {
	t.Helper()
	ws := workspace.NewMemWorkspace()
	repo := NewDocumentRepo(ws, "kb").WithNow(func() time.Time {
		return time.Date(2026, 4, 24, 0, 0, 0, 0, time.UTC)
	})
	return repo, ws
}

func TestDocumentRepo_PutIncrementsVersion(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	doc := knowledge.SourceDocument{DatasetID: "ds1", Name: "a.md", Content: "hello"}

	if err := repo.Put(ctx, doc); err != nil {
		t.Fatalf("first put: %v", err)
	}
	got, err := repo.Get(ctx, "ds1", "a.md")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Version != 1 {
		t.Fatalf("first version = %d, want 1", got.Version)
	}

	doc.Content = "hello v2"
	if err := repo.Put(ctx, doc); err != nil {
		t.Fatalf("second put: %v", err)
	}
	got, err = repo.Get(ctx, "ds1", "a.md")
	if err != nil {
		t.Fatalf("get v2: %v", err)
	}
	if got.Version != 2 {
		t.Fatalf("second version = %d, want 2", got.Version)
	}
	if got.Content != "hello v2" {
		t.Fatalf("content = %q, want hello v2", got.Content)
	}
}

func TestDocumentRepo_GetLosslessContent(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	raw := "---\ntitle: Demo\n---\nbody line one\nbody line two\n"
	if err := repo.Put(ctx, knowledge.SourceDocument{DatasetID: "ds", Name: "doc.md", Content: raw}); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := repo.Get(ctx, "ds", "doc.md")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Content != raw {
		t.Fatalf("content not preserved verbatim:\n got=%q\nwant=%q", got.Content, raw)
	}
}

func TestDocumentRepo_GetMissingReturnsNotFound(t *testing.T) {
	repo, _ := newRepo(t)
	_, err := repo.Get(context.Background(), "ds", "missing.md")
	if !errdefs.IsNotFound(err) {
		t.Fatalf("got %v, want NotFound", err)
	}
}

func TestDocumentRepo_DeleteRemovesSidecars(t *testing.T) {
	repo, ws := newRepo(t)
	ctx := context.Background()
	if err := repo.Put(ctx, knowledge.SourceDocument{DatasetID: "ds", Name: "a.md", Content: "x"}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Drop a sidecar to verify Delete cleans it up.
	if err := ws.Write(ctx, "kb/ds/a.abstract", []byte("L0")); err != nil {
		t.Fatalf("seed sidecar: %v", err)
	}
	if err := repo.Delete(ctx, "ds", "a.md"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	for _, p := range []string{"kb/ds/a.md", "kb/ds/a.meta.json", "kb/ds/a.abstract"} {
		exists, err := ws.Exists(ctx, p)
		if err != nil {
			t.Fatalf("exists %s: %v", p, err)
		}
		if exists {
			t.Fatalf("path %s still exists after delete", p)
		}
	}
}

func TestDocumentRepo_DeleteIsIdempotent(t *testing.T) {
	repo, _ := newRepo(t)
	if err := repo.Delete(context.Background(), "ds", "missing.md"); err != nil {
		t.Fatalf("delete missing: %v", err)
	}
}

func TestDocumentRepo_ListSortedByName(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	for _, n := range []string{"c.md", "a.md", "b.md"} {
		if err := repo.Put(ctx, knowledge.SourceDocument{DatasetID: "ds", Name: n, Content: n}); err != nil {
			t.Fatalf("put %s: %v", n, err)
		}
	}
	docs, err := repo.List(ctx, "ds")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("len(docs) = %d, want 3", len(docs))
	}
	want := []string{"a.md", "b.md", "c.md"}
	for i, d := range docs {
		if d.Name != want[i] {
			t.Fatalf("docs[%d] = %s, want %s", i, d.Name, want[i])
		}
	}
}

func TestDocumentRepo_ListDatasets(t *testing.T) {
	repo, _ := newRepo(t)
	ctx := context.Background()
	for _, ds := range []string{"alpha", "beta", "gamma"} {
		if err := repo.Put(ctx, knowledge.SourceDocument{DatasetID: ds, Name: "x.md", Content: "x"}); err != nil {
			t.Fatalf("put %s: %v", ds, err)
		}
	}
	ids, err := repo.ListDatasets(ctx)
	if err != nil {
		t.Fatalf("listdatasets: %v", err)
	}
	if strings.Join(ids, ",") != "alpha,beta,gamma" {
		t.Fatalf("ids = %v, want sorted alpha/beta/gamma", ids)
	}
}

func TestDocumentRepo_PutValidatesInputs(t *testing.T) {
	repo, _ := newRepo(t)
	if err := repo.Put(context.Background(), knowledge.SourceDocument{DatasetID: "", Name: "x", Content: "y"}); !errdefs.IsValidation(err) {
		t.Fatalf("missing dataset: got %v, want Validation", err)
	}
	if err := repo.Put(context.Background(), knowledge.SourceDocument{DatasetID: "ds", Name: "", Content: "y"}); !errdefs.IsValidation(err) {
		t.Fatalf("missing name: got %v, want Validation", err)
	}
}
