package bbh

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GizClaw/flowcraft/memory/retrieval"
	sdkworkspace "github.com/GizClaw/flowcraft/sdk/workspace"
)

func TestInspectorValidationAndEmptyWorkspace(t *testing.T) {
	if _, err := NewInspector(nil); err == nil {
		t.Fatal("nil workspace should fail")
	}
	if _, err := NewInspector(sdkworkspace.NewMemWorkspace()); err == nil {
		t.Fatal("workspace without Root should fail")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, badgerDir), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewInspector(rootedWorkspace(t, dir)); err == nil {
		t.Fatal("badger file should fail")
	}

	empty := t.TempDir()
	in, err := NewInspector(rootedWorkspace(t, empty))
	if err != nil {
		t.Fatalf("NewInspector empty: %v", err)
	}
	got, err := in.Inspect(context.Background())
	if err != nil {
		t.Fatalf("Inspect empty: %v", err)
	}
	if got.Root != empty || got.BadgerExists || got.BleveExists || got.HNSWExists {
		t.Fatalf("unexpected empty inspection: %+v", got)
	}
	if len(got.Namespaces) != 0 || got.TotalDocs != 0 {
		t.Fatalf("unexpected empty namespaces: %+v", got)
	}
	if err := in.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Inspect(context.Background()); err == nil {
		t.Fatal("inspect after close should fail")
	}
}

func TestInspectorReportsNamespaces(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	idx := openInternalIndex(t, dir, WithConfig(Config{
		HNSW: HNSWConfig{FlushInterval: Duration{Duration: time.Hour}},
	}))
	if err := idx.Upsert(ctx, "runtime/user/agent-a", []retrieval.Doc{
		{ID: "a", Content: "alpha", Vector: []float32{1, 0}, Timestamp: time.Unix(1, 0).UTC()},
		{ID: "b", Content: "beta", Timestamp: time.Unix(2, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Upsert(ctx, "runtime/user/agent-b", []retrieval.Doc{
		{ID: "c", Content: "gamma", Vector: []float32{0, 1}, Timestamp: time.Unix(3, 0).UTC()},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Search(ctx, "empty/shard", retrieval.SearchRequest{QueryText: "nothing"}); err != nil {
		t.Fatal(err)
	}
	if err := idx.Close(); err != nil {
		t.Fatal(err)
	}

	in, err := NewInspector(rootedWorkspace(t, dir))
	if err != nil {
		t.Fatalf("NewInspector: %v", err)
	}
	defer func() {
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	got, err := in.Inspect(ctx)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if !got.BadgerExists || !got.BleveExists || !got.HNSWExists {
		t.Fatalf("expected all storage roots, got %+v", got)
	}
	if got.TotalDocs != 3 || got.TotalVectorDocs != 2 {
		t.Fatalf("totals = docs %d vectors %d, want 3/2", got.TotalDocs, got.TotalVectorDocs)
	}
	if got.PhysicalNamespaceCount != 3 || got.DocNamespaceCount != 2 {
		t.Fatalf("namespace counts = physical %d doc %d, want 3/2", got.PhysicalNamespaceCount, got.DocNamespaceCount)
	}

	a := mustInspectNamespace(t, in, "runtime/user/agent-a")
	if a.DocCount != 2 || a.VectorDocCount != 1 {
		t.Fatalf("agent-a counts = %+v", a)
	}
	if !a.SourceBadger || !a.SourceBleve || !a.SourceHNSW || !a.BleveExists || !a.HNSWExists {
		t.Fatalf("agent-a sources = %+v", a)
	}
	if a.BadgerBytes <= 0 || a.BleveSizeBytes <= 0 || a.HNSWSizeBytes <= 0 {
		t.Fatalf("agent-a sizes = %+v", a)
	}

	empty := mustInspectNamespace(t, in, "empty/shard")
	if empty.DocCount != 0 || !empty.SourceBleve || !empty.Empty {
		t.Fatalf("empty shard = %+v", empty)
	}

	missing, err := in.InspectNamespace(ctx, "missing")
	if err != nil {
		t.Fatal(err)
	}
	if missing.Namespace != "missing" || missing.Token != safeToken("missing") || missing.DocCount != 0 {
		t.Fatalf("missing namespace = %+v", missing)
	}
}

func TestInspectorHandlesInvalidPhysicalToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, bleveDir, "not@base64"), 0o755); err != nil {
		t.Fatal(err)
	}
	in, err := NewInspector(rootedWorkspace(t, dir))
	if err != nil {
		t.Fatalf("NewInspector: %v", err)
	}
	defer func() {
		if err := in.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	got, err := in.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Namespaces) != 1 {
		t.Fatalf("namespaces = %+v", got.Namespaces)
	}
	ns := got.Namespaces[0]
	if ns.Namespace != "not@base64" || ns.DecodeError == "" || !ns.SourceBleve {
		t.Fatalf("invalid token namespace = %+v", ns)
	}
}

func mustInspectNamespace(t *testing.T, in *Inspector, namespace string) NamespaceInspection {
	t.Helper()
	got, err := in.InspectNamespace(context.Background(), namespace)
	if err != nil {
		t.Fatalf("InspectNamespace %s: %v", namespace, err)
	}
	return got
}
